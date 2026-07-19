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
	"github.com/codefly-dev/core/llmout"
	"github.com/codefly-dev/core/resources"
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
	*services.DefaultRuntime
	*pythonservice.Service

	// Persistent Python REPL — started lazily on first `exec` command,
	// torn down by `repl-reset` or plugin shutdown. Lives here on the
	// generic Runtime so every Python specialization shares it.
	replMu sync.Mutex
	repl   *PythonRepl
}

// New builds a generic Python Runtime bound to the shared Service.
func New(svc *pythonservice.Service) *Runtime {
	return &Runtime{
		DefaultRuntime: services.NewDefaultRuntime(svc.Runtime),
		Service:        svc,
	}
}

// Load reads the agent identity, stores environment, and resolves the
// source location. Specializations may override to load additional settings
// (e.g. FastAPI would also parse its own Settings after this).
func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()

	response, err := s.Runtime.LoadService(ctx, req, services.RuntimeLoad{Settings: s.Service.Settings})

	// Prefer the conventional code/ source tree while retaining root-level
	// pyproject checkouts. Code, Runtime, and arbitrary-source adapters then
	// share one source-root contract.
	root := s.Location
	if wd := os.Getenv("CODEFLY_AGENT_WORKDIR"); wd != "" {
		root = wd
	}
	s.Service.SourceLocation = filepath.Join(root, s.Service.Settings.PythonSourceDir())
	if _, statErr := os.Stat(filepath.Join(s.Service.SourceLocation, "pyproject.toml")); statErr != nil {
		if _, rootErr := os.Stat(filepath.Join(root, "pyproject.toml")); rootErr == nil {
			s.Service.SourceLocation = root
		}
	}

	if response != nil && response.Status != nil {
		response.Status.Message = pythonhelpers.AppendRuntimeEvidence(response.Status.Message, pythonhelpers.RuntimeEvidence(s.Service.SourceLocation))
	}
	return response, err
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

	// Formula-driven path: a per-call TestRequest.formula overrides the
	// service's configured Test formula. Either runs the EXACT formula (command
	// + provisioning + output — all DATA, captured by Mind from the project)
	// instead of detecting a runner. The brain stays framework-blind; this
	// plugin is the ONLY place the generic provisioning map becomes uv flags.
	if cmd, output, env, prov, ok := resolveTestFormula(req, s.Base.Service, s.Service.SourceLocation); ok {
		selectors := append([]string{}, req.Filters...)
		if req.Target != "" {
			selectors = append(selectors, req.Target)
		}
		fspec := pythonhelpers.SpecFromFormula(cmd, output, env, prov, selectors)
		s.Wool.Info("running test formula via uv",
			wool.Field("command", fspec.Command), wool.Field("output", fspec.Output))
		fStart := time.Now()
		fRun, fErr := pythonhelpers.RunFormulaStructured(ctx, s.Service.SourceLocation, fspec)
		evidence := pythonhelpers.RuntimeEvidenceForFormula(s.Service.SourceLocation, cmd, output, env, prov, formulaDerivedFromProject(req, s.Base.Service))
		if fRun == nil {
			return s.testErrorWithEvidence(fErr, evidence)
		}
		s.Wool.Forwardf("Tests: %s", fRun.LegacyTestSummary().SummaryLine())
		resp := fRun.ToProtoResponse("formula", req.Suite, time.Since(fStart))
		appendTestResponseEvidence(resp, evidence)
		return resp, fErr
	}

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
		return s.testErrorWithEvidence(runErr, pythonhelpers.RuntimeEvidence(s.Service.SourceLocation))
	}
	resp := run.ToProtoResponse("pytest", req.Suite, duration)
	appendTestResponseEvidence(resp, pythonhelpers.RuntimeEvidence(s.Service.SourceLocation))
	return resp, runErr
}

// Lint runs ruff via the core python helper.
func (s *Runtime) Lint(ctx context.Context, req *runtimev0.LintRequest) (*runtimev0.LintResponse, error) {
	defer s.Wool.Catch()
	if req == nil {
		req = &runtimev0.LintRequest{}
	}

	output, err := pythonhelpers.RunPythonLint(ctx, s.Service.SourceLocation, req.GetTarget())
	compressed := llmout.Compress("ruff", []string{"check"}, output)

	return &runtimev0.LintResponse{
		Status: &runtimev0.LintStatus{
			State:   boolToLintState(err == nil),
			Message: compressed,
		},
		Output: compressed,
	}, nil
}

// Build is a no-op for Python — uv sync handles dep resolution.
// Specializations producing artifacts (containers, wheels) override.
func (s *Runtime) Build(_ context.Context, _ *runtimev0.BuildRequest) (*runtimev0.BuildResponse, error) {
	return &runtimev0.BuildResponse{}, nil
}

// resolveTestFormula picks the test formula to run: a per-call
// TestRequest.formula wins, else the service's language-agnostic config formula
// (resources.Service.Test), else the formula DERIVED from the project, else none
// (ok=false → the agent's default runner). Returns the raw language-agnostic
// fields; SpecFromFormula does the uv mapping.
//
// A per-call formula with NO command but WITH provisioning/env (a Mind
// environment heal like {cwd: tests} or {with: Werkzeug<3}) is an OVERLAY: it
// rides on top of the configured/derived formula instead of being dropped.
// Mirrors Mind's InProcessRuntime so local == remote — before this, a healed
// work_dir sent without a command was a silent no-op on the agent path.
func resolveTestFormula(req *runtimev0.TestRequest, svc *resources.Service, sourceDir string) (cmd []string, output string, env, prov map[string]string, ok bool) {
	f := req.GetFormula()
	if f != nil && len(f.GetCommand()) > 0 {
		return f.GetCommand(), f.GetOutput(), f.GetEnv(), enrichSuppliedProvisioning(f.GetProvisioning(), sourceDir), true
	}
	overlay := func(bcmd []string, boutput string, benv, bprov map[string]string) ([]string, string, map[string]string, map[string]string, bool) {
		if f != nil {
			if o := f.GetOutput(); o != "" {
				boutput = o
			}
			benv = overlayStringMap(benv, f.GetEnv())
			bprov = overlayStringMap(bprov, f.GetProvisioning())
		}
		return bcmd, boutput, benv, bprov, true
	}
	if svc != nil && svc.Test != nil && len(svc.Test.Command) > 0 {
		return overlay(svc.Test.Command, svc.Test.Output, svc.Test.Env, enrichSuppliedProvisioning(svc.Test.Provisioning, sourceDir))
	}
	// No explicit (per-call) or configured (service.yaml) formula → DERIVE it from
	// the project: read its own declarations (tox/Makefile/CI/README) for the
	// command + its packaging metadata for provisioning (editable, python,
	// requirements). This is what lets Mind send a formula-less Test and have the
	// python plugin "just run the project's tests" — no framework knowledge in Mind.
	if sourceDir != "" {
		if dcmd, doutput, denv, dprov, dok := pythonhelpers.DeriveFormula(sourceDir); dok {
			return overlay(dcmd, doutput, denv, dprov)
		}
	}
	return nil, "", nil, nil, false
}

// overlayStringMap returns base with overlay's entries layered on top (overlay
// wins). Used so a per-call heal (provisioning/env without a command) survives
// when the command comes from config/derivation. Returns base unchanged when
// the overlay is empty.
func overlayStringMap(base, overlay map[string]string) map[string]string {
	if len(overlay) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// enrichSuppliedProvisioning delegates to the SHARED core helper
// (pythonhelpers.EnrichSuppliedProvisioning) so the agent's Test RPC and
// Mind's in-process runtime resolve identical formulas — the health probe and
// the task-phase test run must see the same derived editable/python/
// build-requires under any supplied keys.
func enrichSuppliedProvisioning(supplied map[string]string, sourceDir string) map[string]string {
	return pythonhelpers.EnrichSuppliedProvisioning(supplied, sourceDir)
}

func formulaDerivedFromProject(req *runtimev0.TestRequest, svc *resources.Service) bool {
	if f := req.GetFormula(); f != nil && len(f.GetCommand()) > 0 {
		return false
	}
	return svc == nil || svc.Test == nil || len(svc.Test.Command) == 0
}

func (s *Runtime) testErrorWithEvidence(err error, evidence string) (*runtimev0.TestResponse, error) {
	if err == nil {
		msg := pythonhelpers.AppendRuntimeEvidence("test runtime errored before tests could execute", evidence)
		return &runtimev0.TestResponse{
			Status: &runtimev0.TestStatus{State: runtimev0.TestStatus_ERROR, Message: msg},
			Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_ERRORED, Message: msg},
			Counts: &runtimev0.TestCounts{},
		}, nil
	}
	resp, testErr := s.Runtime.TestError(err)
	appendTestResponseErrorEvidence(resp, evidence)
	return resp, testErr
}

func appendTestResponseEvidence(resp *runtimev0.TestResponse, evidence string) {
	if resp == nil || resp.GetResult().GetState() != runtimev0.TestRunResult_ERRORED {
		return
	}
	appendTestResponseErrorEvidence(resp, evidence)
}

func appendTestResponseErrorEvidence(resp *runtimev0.TestResponse, evidence string) {
	if resp == nil {
		return
	}
	if resp.Counts == nil {
		resp.Counts = &runtimev0.TestCounts{}
	}
	if resp.Status != nil {
		resp.Status.Message = pythonhelpers.AppendRuntimeEvidence(resp.Status.Message, evidence)
	}
	if resp.Result != nil {
		resp.Result.Message = pythonhelpers.AppendRuntimeEvidence(resp.Result.Message, evidence)
	} else {
		resp.Result = &runtimev0.TestRunResult{
			State:   runtimev0.TestRunResult_ERRORED,
			Message: pythonhelpers.AppendRuntimeEvidence("", evidence),
		}
	}
}

func boolToLintState(ok bool) runtimev0.LintStatus_Status {
	if ok {
		return runtimev0.LintStatus_SUCCESS
	}
	return runtimev0.LintStatus_ERROR
}
