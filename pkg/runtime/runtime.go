// Package runtime implements the generic Python Runtime gRPC service.
// Specializations embed *Runtime and override methods (typically Start,
// sometimes Init or Test) to add protocol-specific lifecycle.
package runtime

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/codefly-dev/core/agents/services"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	runners "github.com/codefly-dev/core/runners/base"
	pythonhelpers "github.com/codefly-dev/core/runners/python"
	"github.com/codefly-dev/core/wool"

	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

// Runtime is the generic Python runtime server. Embedded by specializations
// to inherit uv-based dep sync, pytest, ruff lint, the no-op Build, and
// the persistent Python REPL commands defined in commands.go.
//
// *pythonservice.Service is embedded (not held as a named field) so that
// services.Base methods — Wool, Logger, Location, s.Runtime, s.Base.Load —
// promote through. Specializations reuse this same trick.
//
// Specializations inherit the REPL machinery by embedding *Runtime and
// calling RegisterReplCommands once from their own Load — same pattern
// go-grpc uses for its explicit registerCommands call.
type Runtime struct {
	services.RuntimeServer
	*pythonservice.Service

	// Persistent Python REPL — started lazily on first `exec` command,
	// torn down by `repl-reset` or plugin shutdown. Lives here on the
	// generic Runtime so every Python specialization shares it.
	replMu sync.Mutex
	repl   *PythonRepl
}

// New builds a generic Python Runtime bound to the shared Service.
func New(svc *pythonservice.Service) *Runtime {
	return &Runtime{Service: svc}
}

// Load reads the agent identity, stores environment, and resolves the
// source location. Specializations may override to load additional settings
// (e.g. FastAPI would also parse its own Settings after this).
func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()

	if err := s.Base.Load(ctx, req.Identity, s.Service.Settings); err != nil {
		return s.Runtime.LoadErrorf(err, "loading base")
	}

	s.Runtime.SetEnvironment(req.Environment)

	// Python convention: source is the service directory itself.
	s.Service.SourceLocation = s.Location
	if wd := os.Getenv("CODEFLY_AGENT_WORKDIR"); wd != "" {
		s.Service.SourceLocation = wd
	}

	return s.Runtime.LoadResponse()
}

// Init runs `uv sync` to materialize the venv. Non-fatal on failure.
// Specializations may override to add their own init (e.g. collecting
// network mappings, wiring env vars) — they should still call this first.
func (s *Runtime) Init(ctx context.Context, _ *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()

	// Route `uv sync` through the plugin's active RunnerEnvironment so
	// Docker/Nix/native mode stays consistent. Fallback to fresh native
	// when ActiveEnv is nil (generic Runtime.Init runs before a
	// specialization has called CreateRunnerEnvironment — rare, but the
	// fallback keeps the generic path usable standalone).
	env := s.Service.ActiveEnv
	if env == nil {
		native, envErr := runners.NewNativeEnvironment(ctx, s.Service.SourceLocation)
		if envErr != nil {
			s.Wool.Warn("uv sync skipped — cannot create runner environment", nil)
			return s.Runtime.InitResponse()
		}
		env = native
	}
	proc, procErr := env.NewProcess("uv", "sync")
	if procErr != nil {
		s.Wool.Warn("uv sync skipped — cannot create process", nil)
		return s.Runtime.InitResponse()
	}
	proc.WithOutput(s.Logger)
	if err := proc.Run(ctx); err != nil {
		s.Wool.Warn("uv sync failed (non-fatal)", nil)
	}

	return s.Runtime.InitResponse()
}

// Start is a no-op on the generic runtime — a bare Python "service"
// without an entrypoint has nothing to run. Specializations override.
func (s *Runtime) Start(_ context.Context, _ *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	return s.Runtime.StartResponse()
}

// Stop is a no-op on the generic runtime.
func (s *Runtime) Stop(_ context.Context, _ *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	return s.Runtime.StopResponse()
}

// Destroy is a no-op on the generic runtime.
func (s *Runtime) Destroy(_ context.Context, _ *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	return s.Runtime.DestroyResponse()
}

// Test runs pytest via the core python helper and returns the structured
// summary. Streams per-test events to the logger as they arrive (TUI
// shows live progress) and persists raw output to <SourceLocation>/.cache/
// last-test.txt for post-mortem.
//
// Specializations should NOT override this unless their layer has extra
// setup (e.g. fixtures) beyond what Init already did.
func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Info("running python tests",
		wool.Field("target", req.Target),
		wool.Field("filters", req.Filters),
		wool.Field("coverage", req.Coverage),
		wool.Field("timeout", req.Timeout),
		wool.Field("extra_args", req.ExtraArgs))

	opts := pythonhelpers.TestOptions{
		// Stream per-test events through the logger so the CLI TUI shows
		// live RUN/PASS/FAIL feedback instead of waiting for the summary.
		OnEvent: func(ev pythonhelpers.TestEvent) {
			switch ev.Action {
			case "pass":
				s.Wool.Forwardf("PASS %s", ev.Test)
			case "fail":
				s.Wool.Forwardf("FAIL %s", ev.Test)
			case "skip":
				s.Wool.Forwardf("SKIP %s", ev.Test)
			}
		},
		// Persist raw output for post-mortem. Best-effort.
		CacheDir: filepath.Join(s.Service.SourceLocation, ".cache"),

		// Forward CLI flags to pytest.
		Target:     req.Target,
		Filters:    req.Filters,
		Verbose:    req.Verbose,
		VerboseSet: true,
		Timeout:    req.Timeout,
		Coverage:   req.Coverage,
		ExtraArgs:  req.ExtraArgs,
	}

	started := time.Now()
	run, runErr := pythonhelpers.RunPythonTestsStructured(ctx, s.Service.SourceLocation, nil, opts)
	duration := time.Since(started)

	if run != nil {
		// One-line summary in the agent log. Per-failure detail lives
		// in the structured response — we deliberately do NOT dump
		// captured_output to the log; that's the size-discipline win.
		s.Wool.Forwardf("Tests: %s", run.LegacyTestSummary().SummaryLine())
	}

	if run == nil {
		return s.Runtime.TestError(runErr)
	}
	return run.ToProtoResponse("pytest", req.Suite, duration), runErr
}

// Lint runs ruff via the core python helper.
func (s *Runtime) Lint(ctx context.Context, _ *runtimev0.LintRequest) (*runtimev0.LintResponse, error) {
	defer s.Wool.Catch()

	output, err := pythonhelpers.RunPythonLint(ctx, s.Service.SourceLocation)

	return &runtimev0.LintResponse{
		Status: &runtimev0.LintStatus{
			State:   boolToLintState(err == nil),
			Message: output,
		},
	}, nil
}

// Build is a no-op for Python — uv sync handles dep resolution.
// Specializations producing artifacts (containers, wheels) override.
func (s *Runtime) Build(_ context.Context, _ *runtimev0.BuildRequest) (*runtimev0.BuildResponse, error) {
	return &runtimev0.BuildResponse{}, nil
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func boolToLintState(ok bool) runtimev0.LintStatus_Status {
	if ok {
		return runtimev0.LintStatus_SUCCESS
	}
	return runtimev0.LintStatus_ERROR
}
