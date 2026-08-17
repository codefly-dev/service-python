// Package runtime implements the generic Python Runtime gRPC service.
// Specializations embed *Runtime and override methods (typically Start,
// sometimes Init or Test) to add protocol-specific lifecycle.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codefly-dev/core/agents/services"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/llmout"
	"github.com/codefly-dev/core/resources"
	pythonhelpers "github.com/codefly-dev/core/runners/python"
	selectioncontract "github.com/codefly-dev/core/runners/testselection"
	"github.com/codefly-dev/core/wool"

	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

// Runtime is the generic Python runtime server. Embedded by specializations
// to inherit externally provisioned pytest, ruff lint, syntax Build, and
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
	s.Service.SourceLocation = s.Service.ResolveSourceLocation()

	if response != nil && response.Status != nil {
		evidence := runtimeEvidenceWithConfiguredOverrides(pythonhelpers.RuntimeEvidence(s.Service.SourceLocation), s.Base.Service)
		response.Status.Message = pythonhelpers.AppendRuntimeEvidence(response.Status.Message, evidence)
	}
	return response, err
}

// Init deliberately does not materialize dependencies in the source checkout.
// Test formulas provision their environment through uv's external cache, and
// specializations may create a runner environment without writing .venv or
// uv.lock into the project. This keeps initialization safe for read-only
// control-plane operations such as source inspection and linting.
func (s *Runtime) Init(_ context.Context, _ *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
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
// summary. Streams per-test events to the logger as they arrive (TUI shows live
// progress). Runtime evidence stays outside the project checkout.
//
// Specializations should NOT override this unless their layer has extra
// setup (e.g. fixtures) beyond what Init already did.
func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	defer s.Wool.Catch()
	if err := selectioncontract.ValidateRequest(req); err != nil {
		return nil, fmt.Errorf("python runtime: invalid test request: %w", err)
	}

	s.Wool.Info("running python tests",
		wool.Field("target", req.Target),
		wool.Field("filters", req.Filters),
		wool.Field("coverage", req.Coverage),
		wool.Field("timeout", req.Timeout),
		wool.Field("extra_args", req.ExtraArgs),
		wool.Field("source_location", s.Service.SourceLocation),
		wool.Field("configured_env_keys", configuredTestEnvKeys(s.Base.Service)))

	// Formula-driven path: a per-call TestRequest.formula overrides the
	// service's configured Test formula. Either runs the EXACT formula (command
	// + provisioning + output — all DATA, captured by Mind from the project)
	// instead of detecting a runner. The brain stays framework-blind; this
	// plugin is the ONLY place the generic provisioning map becomes uv flags.
	if cmd, output, env, prov, ok := resolveTestFormula(req, s.Base.Service, s.Service.SourceLocation); ok {
		fspec := pythonhelpers.SpecFromFormula(cmd, output, env, prov, nil)
		if selectorErr := applyPythonFormulaTestScope(req, &fspec); selectorErr != nil {
			return nil, fmt.Errorf("python runtime selection: %w", selectorErr)
		}
		s.Wool.Info("running test formula via uv",
			wool.Field("command", fspec.Command),
			wool.Field("selectors", fspec.Selectors),
			wool.Field("output", fspec.Output))
		fStart := time.Now()
		fRun, fErr := pythonhelpers.RunFormulaStructured(ctx, s.Service.SourceLocation, fspec)
		evidence := pythonhelpers.RuntimeEvidenceForFormula(s.Service.SourceLocation, cmd, output, env, prov, formulaDerivedFromProject(req, s.Base.Service))
		evidence = runtimeEvidenceWithConfiguredOverrides(evidence, s.Base.Service)
		if fRun == nil {
			resp, rpcErr := s.testErrorWithEvidence(fErr, evidence)
			ensureTestRun(resp, "formula", req.GetSuite())
			return acknowledgeTestSelection(req, resp, rpcErr)
		}
		resp := fRun.ToProtoResponse("formula", req.Suite, time.Since(fStart))
		s.Wool.Forwardf("Tests: %s", testResponseLogSummary(resp))
		appendTestResponseEvidence(resp, evidence)
		// A normal failing test process exits non-zero. Its structured response
		// is the operation result, not a gRPC transport error; returning fErr
		// would make gRPC discard the response and its selection acknowledgement.
		return acknowledgeTestSelection(req, resp, nil)
	}
	s.Wool.Info("no test formula detected; using the Python default test runner",
		wool.Field("runtime_evidence", pythonhelpers.RuntimeEvidence(s.Service.SourceLocation)))

	target, filters, selectorErr := pythonDefaultTestScope(req)
	if selectorErr != nil {
		return nil, fmt.Errorf("python runtime selection: %w", selectorErr)
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
		// Forward CLI flags to pytest.
		Target:     target,
		Filters:    filters,
		Verbose:    req.Verbose,
		VerboseSet: true,
		Timeout:    req.Timeout,
		Coverage:   req.Coverage,
		ExtraArgs:  req.ExtraArgs,
	}

	started := time.Now()
	run, runErr := pythonhelpers.RunPythonTestsStructured(
		ctx,
		s.Service.SourceLocation,
		configuredTestEnvironment(s.Base.Service),
		opts,
	)
	duration := time.Since(started)

	if run == nil {
		evidence := runtimeEvidenceWithConfiguredOverrides(pythonhelpers.RuntimeEvidence(s.Service.SourceLocation), s.Base.Service)
		resp, rpcErr := s.testErrorWithEvidence(runErr, evidence)
		ensureTestRun(resp, "pytest", req.GetSuite())
		return acknowledgeTestSelection(req, resp, rpcErr)
	}

	evidence := runtimeEvidenceWithConfiguredOverrides(pythonhelpers.RuntimeEvidence(s.Service.SourceLocation), s.Base.Service)
	resp, rpcErr := s.finalizeDefaultTestResponse(req, run, runErr, duration, evidence)
	// Log only the final response selected for the caller. In particular, a
	// zero-case runner error must not log the provisional parser default
	// ("all tests passed") before being classified as an environment error.
	s.Wool.Forwardf("Tests: %s", testResponseLogSummary(resp))
	return resp, rpcErr
}

// finalizeDefaultTestResponse selects the one typed response observed by both
// the live agent log and the RPC caller. A non-nil runner error with zero
// executed cases is an execution-environment failure, not the parser's
// provisional zero-count pass. Ordinary test failures retain their structured
// suites and remain operation results rather than gRPC transport errors.
func (s *Runtime) finalizeDefaultTestResponse(req *runtimev0.TestRequest, run *pythonhelpers.StructuredTestRun, runErr error, duration time.Duration, evidence string) (*runtimev0.TestResponse, error) {
	if runErr != nil && run.LegacyTestSummary().Run == 0 {
		resp, rpcErr := s.testErrorWithEvidence(runErr, evidence)
		ensureTestRun(resp, "pytest", req.GetSuite())
		return acknowledgeTestSelection(req, resp, rpcErr)
	}
	resp := run.ToProtoResponse("pytest", req.GetSuite(), duration)
	appendTestResponseEvidence(resp, evidence)
	return acknowledgeTestSelection(req, resp, nil)
}

func configuredTestEnvKeys(service *resources.Service) []string {
	if service == nil || service.Test == nil || len(service.Test.Env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(service.Test.Env))
	for key := range service.Test.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// runtimeEvidenceWithConfiguredOverrides makes provenance actionable for the
// environment healer. Effective provisioning alone cannot tell the caller
// whether a value came from project derivation or from a prior persisted
// experiment. Naming the explicit paths lets ConfigChange.UNSET restore the
// plugin-derived baseline instead of accumulating contradictory overrides.
func runtimeEvidenceWithConfiguredOverrides(evidence string, service *resources.Service) string {
	if service == nil || service.Test == nil {
		return evidence
	}
	paths := make([]string, 0, 2+len(service.Test.Provisioning)+len(service.Test.Env))
	if len(service.Test.Command) > 0 {
		paths = append(paths, "test.command")
	}
	if service.Test.Output != "" {
		paths = append(paths, "test.output")
	}
	for key := range service.Test.Provisioning {
		paths = append(paths, "test.provisioning."+key)
	}
	for key := range service.Test.Env {
		paths = append(paths, "test.env."+key)
	}
	if len(paths) == 0 {
		return evidence
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString(strings.TrimRight(evidence, "\n"))
	b.WriteString("\n  persisted_override_paths (take precedence over project-derived defaults):\n")
	for _, path := range paths {
		b.WriteString("    - " + path + "\n")
	}
	b.WriteString("  override_reset: apply ConfigChange UNSET to a persisted path to restore plugin derivation")
	return b.String()
}

// configuredTestEnvironment projects the persisted, plugin-owned test
// environment into the shared default runner. Formula-driven runs already
// receive this map through SpecFromFormula; the default runner must honor the
// same configuration contract instead of silently dropping recovery state.
func configuredTestEnvironment(service *resources.Service) []*resources.EnvironmentVariable {
	keys := configuredTestEnvKeys(service)
	if len(keys) == 0 {
		return nil
	}
	variables := make([]*resources.EnvironmentVariable, 0, len(keys))
	for _, key := range keys {
		variables = append(variables, &resources.EnvironmentVariable{Key: key, Value: service.Test.Env[key]})
	}
	return variables
}

// testResponseLogSummary renders the structured runtime verdict rather than
// the legacy case counters alone. Zero-case environment failures and healthy
// materialization probes both have zero passing tests; logging either as
// "0 passed" discards the state a developer needs to diagnose the run.
func testResponseLogSummary(response *runtimev0.TestResponse) string {
	if response == nil {
		return "runtime returned no structured test response"
	}
	result := response.GetResult()
	counts := response.GetCounts()
	message := strings.TrimSpace(result.GetMessage())
	if result.GetState() == runtimev0.TestRunResult_ERRORED ||
		result.GetState() == runtimev0.TestRunResult_TIMED_OUT ||
		counts.GetTotal() == 0 || strings.HasPrefix(message, "env-blocked") {
		if message != "" {
			return boundedTestLogMessage(message)
		}
		return strings.ToLower(result.GetState().String())
	}
	parts := []string{fmt.Sprintf("%d passed", counts.GetPassed())}
	if counts.GetFailed() > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", counts.GetFailed()))
	}
	if counts.GetErrored() > 0 {
		parts = append(parts, fmt.Sprintf("%d errored", counts.GetErrored()))
	}
	if counts.GetSkipped() > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", counts.GetSkipped()))
	}
	if coverage := response.GetCoverage().GetTotalPct(); coverage > 0 {
		parts = append(parts, fmt.Sprintf("%.1f%% coverage", coverage))
	}
	return strings.Join(parts, ", ")
}

const (
	maxTestLogMessageBytes  = 4_096
	testLogMessageHeadBytes = 1_200
	testLogMessageTailBytes = 2_600
)

// boundedTestLogMessage keeps the live operator stream useful during large
// native build failures. The complete diagnostic remains untouched in the
// typed TestResponse consumed by Mind; only the redundant Wool projection is
// bounded. Keeping both the classification/header and the terminal stderr is
// intentional: compiler and linker causes are normally at the end.
func boundedTestLogMessage(message string) string {
	if len(message) <= maxTestLogMessageBytes {
		return message
	}
	headEnd := testLogMessageHeadBytes
	// Back off only to the nearest rune boundary at the cut point; do NOT
	// revalidate the whole prefix. Native diagnostics can carry invalid UTF-8
	// bytes anywhere in the head, and a full-prefix check would drag headEnd
	// back past them, discarding the classification header we mean to keep.
	for headEnd > 0 && !utf8.RuneStart(message[headEnd]) {
		headEnd--
	}
	tailStart := len(message) - testLogMessageTailBytes
	for tailStart < len(message) && !utf8.RuneStart(message[tailStart]) {
		tailStart++
	}
	omitted := tailStart - headEnd
	return message[:headEnd] + fmt.Sprintf(
		"\n... [%d bytes omitted from live log; full diagnostic retained in typed TestResponse] ...\n",
		omitted,
	) + message[tailStart:]
}

// applyPythonFormulaTestScope translates the language-neutral runtime request
// into runner-private formula arguments. This is the boundary that owns the
// distinction between pytest collection targets and pytest name expressions:
// callers provide target/filter data and never construct runner flags.
//
// ARCHITECTURE: A target or exact pytest node identity is authoritative scope.
// Derived formulas may contain broad discovery operands such as
// `pytest --pyargs astropy docs`; retaining those operands would execute
// unrelated tests in addition to the requested target. Exact scope therefore
// replaces broad discovery, while ordinary filters preserve the derived scope
// and become pytest's name-expression argument. Typed TestSelection remains
// the acknowledged, fail-closed acceptance contract.
func applyPythonFormulaTestScope(req *runtimev0.TestRequest, spec *pythonhelpers.TestFormulaSpec) error {
	if req.GetSelection() != nil {
		selectors, err := pythonhelpers.RenderTestSelection(req.GetSelection(), spec.Command, spec.Cwd)
		if err != nil {
			return err
		}
		spec.Command = pythonhelpers.CommandForExactSelection(spec.Command)
		spec.Selectors = selectors
		return nil
	}

	// JUnit XML selects Codefly's pytest adapter. The Python plugin, not Mind,
	// owns pytest's private distinction between positional collection targets
	// and -k name expressions.
	if spec.Output == pythonhelpers.OutputJUnitXML {
		target := strings.TrimSpace(req.GetTarget())
		var exactSelectors, relativeNodeSelectors, namePatterns []string
		for _, filter := range req.GetFilters() {
			if isRelativePytestNodeSelector(filter) {
				relativeNodeSelectors = append(relativeNodeSelectors, strings.TrimSpace(filter))
			} else if isExactPytestSelector(filter) {
				exactSelectors = append(exactSelectors, filter)
			} else if strings.TrimSpace(filter) != "" {
				namePatterns = append(namePatterns, filter)
			}
		}
		if len(relativeNodeSelectors) > 0 {
			if !isPytestFileTarget(target) {
				return fmt.Errorf("Python relative node selectors %q require one .py target, got %q", relativeNodeSelectors, target)
			}
			if len(exactSelectors) > 0 || len(namePatterns) > 0 {
				return fmt.Errorf("Python relative node selectors cannot be mixed with independent exact selectors or name filters; use a typed TestSelection")
			}
			for _, selector := range relativeNodeSelectors {
				exactSelectors = append(exactSelectors, target+"::"+strings.TrimPrefix(selector, "::"))
			}
			target = ""
		}
		nameFilters, err := normalizePytestNameFilters(namePatterns)
		if err != nil {
			return err
		}
		if target != "" {
			exactSelectors = append(exactSelectors, target)
		}
		if len(exactSelectors) > 0 {
			spec.Command = pythonhelpers.CommandForExactSelection(spec.Command)
		}
		if len(nameFilters) > 0 {
			spec.ExtraArgs = append(spec.ExtraArgs, "-k", strings.Join(nameFilters, " or "))
			// A filter-only request is a complete test execution even though it
			// has no positional selector. Say so explicitly: formula Core's auto
			// mode intentionally treats a selector-free invocation as an
			// environment materialization probe.
			spec.ExecutionMode = pythonhelpers.FormulaExecutionComplete
		}
		spec.Selectors = exactSelectors
		return nil
	}

	// Non-pytest formula runners own their positional selector grammar. Core's
	// formula executor performs the runner-specific normalization (for example,
	// unittest display identities to Django dotted labels).
	spec.Selectors = append(spec.Selectors, req.GetFilters()...)
	if target := strings.TrimSpace(req.GetTarget()); target != "" {
		spec.Selectors = append(spec.Selectors, target)
	}
	return nil
}

// isExactPytestSelector recognizes collection identities, not arbitrary name
// expressions. This knowledge belongs in the Python runtime plugin. SWE-bench
// node IDs and file targets are positional pytest operands; simple names and
// boolean expressions remain filters.
func isExactPytestSelector(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, "::") {
		return true
	}
	normalized := strings.ReplaceAll(value, `\`, "/")
	return strings.HasSuffix(normalized, ".py") || strings.Contains(normalized, ".py/")
}

// isRelativePytestNodeSelector recognizes the pytest identity suffix callers
// naturally pair with TestRequest.target: `Class::method`. It is not a valid
// positional operand on its own. The runtime joins it to the file target so an
// identical logical request cannot accidentally execute two unrelated
// selectors and collect zero cases.
func isRelativePytestNodeSelector(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "::") {
		return false
	}
	return !isPytestFileTarget(strings.SplitN(value, "::", 2)[0])
}

func isPytestFileTarget(value string) bool {
	value = strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
	return value != "" && !strings.Contains(value, "::") && strings.HasSuffix(value, ".py")
}

func pythonDefaultTestScope(req *runtimev0.TestRequest) (string, []string, error) {
	if req.GetSelection() == nil {
		target := strings.TrimSpace(req.GetTarget())
		namePatterns := make([]string, 0, len(req.GetFilters()))
		var relativeNode string
		for _, filter := range req.GetFilters() {
			if isRelativePytestNodeSelector(filter) {
				if relativeNode != "" {
					return "", nil, fmt.Errorf("Python default test request has multiple relative node selectors; use one typed TestSelection")
				}
				relativeNode = strings.TrimSpace(filter)
				continue
			}
			if !isExactPytestSelector(filter) {
				namePatterns = append(namePatterns, filter)
				continue
			}
			if target != "" {
				return "", nil, fmt.Errorf("Python test request has multiple exact targets %q and %q; use one typed TestSelection", target, filter)
			}
			target = strings.TrimSpace(filter)
		}
		if relativeNode != "" {
			if !isPytestFileTarget(target) || len(namePatterns) > 0 {
				return "", nil, fmt.Errorf("Python relative node selector %q requires one .py target and no name filters; use a typed TestSelection", relativeNode)
			}
			target += "::" + strings.TrimPrefix(relativeNode, "::")
		}
		filters, err := normalizePytestNameFilters(namePatterns)
		return target, filters, err
	}
	selectors, err := pythonhelpers.RenderTestSelection(req.GetSelection(), []string{"pytest"}, "")
	if err != nil {
		return "", nil, err
	}
	if len(selectors) != 1 {
		return "", nil, fmt.Errorf("typed Python selection rendered %d targets, want exactly one", len(selectors))
	}
	return selectors[0], nil, nil
}

func ensureTestRun(resp *runtimev0.TestResponse, runner, suite string) {
	if resp != nil && resp.Run == nil {
		resp.Run = &runtimev0.TestRun{Runner: runner, SuiteName: suite}
	}
}

func acknowledgeTestSelection(req *runtimev0.TestRequest, resp *runtimev0.TestResponse, runErr error) (*runtimev0.TestResponse, error) {
	if ackErr := selectioncontract.Acknowledge(req, resp); ackErr != nil {
		return resp, errors.Join(runErr, fmt.Errorf("acknowledge typed test selection: %w", ackErr))
	}
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

// Build is Python's read-only compile gate. Packaging remains separate, but a
// Build RPC must never claim success for syntactically invalid source.
func (s *Runtime) Build(ctx context.Context, req *runtimev0.BuildRequest) (*runtimev0.BuildResponse, error) {
	if req == nil {
		req = &runtimev0.BuildRequest{}
	}
	output, err := pythonhelpers.RunPythonBuild(ctx, s.Service.SourceLocation, req.GetTarget())
	if err != nil {
		return s.Runtime.BuildErrorf(err, "python compile failed:\n%s", output)
	}
	return s.Runtime.BuildResponse(output)
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
			// A persisted service formula may intentionally contain only an
			// environment/provisioning repair. Keep the project-derived command,
			// then layer plugin-owned configuration over it before the per-call
			// request overlay. Precedence is request > service config > project.
			if svc != nil && svc.Test != nil {
				if svc.Test.Output != "" {
					doutput = svc.Test.Output
				}
				denv = overlayStringMap(denv, svc.Test.Env)
				dprov = overlayStringMap(dprov, svc.Test.Provisioning)
			}
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
	maps.Copy(out, base)
	maps.Copy(out, overlay)
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
	if resp == nil {
		return
	}
	result := resp.GetResult()
	if result.GetState() != runtimev0.TestRunResult_ERRORED && !strings.Contains(result.GetMessage(), "env-blocked (") {
		return
	}
	evidence = runtimeEvidenceWithDiagnosticRecovery(evidence, result.GetMessage())
	appendTestResponseErrorEvidence(resp, evidence)
}

// runtimeEvidenceWithDiagnosticRecovery turns a plugin-classified runtime
// block into an exact Configure action when the Python runner owns a safe,
// narrow repair. Mind remains framework-blind: it receives a dotted path,
// operation, and value from this plugin instead of inventing pytest flags.
//
// Runtime-data failures are warning-as-error verdicts from historical project
// data (time tables, certificate bundles, schema caches), not candidate-code
// failures. The runner already proves that every failing case terminates on
// the same freshness warning before it emits runtime-data-stale. Preserve the
// project's test scope and source install, and suppress only that diagnosed
// warning category/message for this runtime environment.
func runtimeEvidenceWithDiagnosticRecovery(evidence, message string) string {
	const marker = "env-blocked (runtime-data-stale):"
	if !strings.Contains(message, marker) || strings.Contains(evidence, "diagnostic_supported_recovery:") {
		return evidence
	}
	category, warningMessage, ok := runtimeDataWarningIdentity(message)
	if !ok {
		return evidence
	}
	filter := fmt.Sprintf("-W %q", "ignore:"+warningMessage+":"+category)
	value := filter
	if current := runtimeEvidenceEnvValue(evidence, "PYTEST_ADDOPTS"); current != "" && !strings.Contains(current, filter) {
		value = strings.TrimSpace(current + " " + filter)
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(evidence, "\n"))
	b.WriteString("\n  diagnostic_supported_recovery:\n")
	b.WriteString("    reason: runtime-data-stale\n")
	b.WriteString("    path: test.env.PYTEST_ADDOPTS\n")
	b.WriteString("    operation: SET\n")
	b.WriteString("    value: " + value + "\n")
	b.WriteString("    proof: rerun the unchanged requested test scope; Configure persistence alone is not proof")
	return b.String()
}

func runtimeDataWarningIdentity(message string) (category, warningMessage string, ok bool) {
	parts := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), ": ")
	for i := 0; i+1 < len(parts); i++ {
		candidate := strings.TrimSpace(parts[i])
		if !validDottedWarningCategory(candidate) {
			continue
		}
		detail := strings.TrimSpace(parts[i+1])
		if cut, _, found := strings.Cut(detail, " due to "); found {
			detail = strings.TrimSpace(cut)
		}
		if cut, _, found := strings.Cut(detail, "\n"); found {
			detail = strings.TrimSpace(cut)
		}
		if detail == "" || len(detail) > 240 || strings.ContainsAny(detail, ":\r\n\"") {
			return "", "", false
		}
		return candidate, detail, true
	}
	return "", "", false
}

func validDottedWarningCategory(value string) bool {
	if !strings.HasSuffix(value, "Warning") || !strings.Contains(value, ".") {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if part == "" {
			return false
		}
		for i, r := range part {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '_' && (i == 0 || r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

func runtimeEvidenceEnvValue(evidence, key string) string {
	inEnv := false
	for _, line := range strings.Split(evidence, "\n") {
		switch {
		case line == "  env:":
			inEnv = true
			continue
		case inEnv && !strings.HasPrefix(line, "    "):
			return ""
		case inEnv:
			name, value, found := strings.Cut(strings.TrimSpace(line), ": ")
			if found && name == key {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func appendTestResponseErrorEvidence(resp *runtimev0.TestResponse, evidence string) {
	if resp == nil {
		return
	}
	if resp.Counts == nil {
		resp.Counts = &runtimev0.TestCounts{}
	}
	// The legacy TestStatus mirror is deprecated, but core's RuntimeWrapper.TestError
	// still populates ONLY Status (Result is left nil) — so on the error path this is
	// the sole carrier of the runner's message. Keep appending evidence here until the
	// upstream error builder emits the structured Result; dropping it would strip the
	// diagnostic from that response.
	//lint:ignore SA1019 legacy error carrier; see comment above
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
