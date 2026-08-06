package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	pythonhelpers "github.com/codefly-dev/core/runners/python"
)

func TestResponseLogSummaryPreservesStructuredRuntimeState(t *testing.T) {
	t.Run("environment block", func(t *testing.T) {
		run := &pythonhelpers.StructuredTestRun{
			EnvError: &pythonhelpers.RunEnvError{
				Reason: pythonhelpers.EnvErrorNoTestsExecuted,
				Detail: "test command executed zero tests",
			},
		}
		got := testResponseLogSummary(run.ToProtoResponse("formula", "", time.Second))
		if !strings.Contains(got, "env-blocked (no-tests-executed)") || strings.Contains(got, "0 passed") {
			t.Fatalf("summary = %q, want the typed environment block", got)
		}
	})

	t.Run("materialized probe", func(t *testing.T) {
		run := &pythonhelpers.StructuredTestRun{Materialized: true}
		got := testResponseLogSummary(run.ToProtoResponse("formula", "", time.Second))
		if !strings.Contains(got, pythonhelpers.EnvMaterializedMessagePrefix) || strings.Contains(got, "0 passed") {
			t.Fatalf("summary = %q, want the typed materialization result", got)
		}
	})
}

func TestResponseLogSummaryBoundsNativeDiagnosticsButKeepsTerminalCause(t *testing.T) {
	prefix := "env-blocked (provisioning-failed): editable project install failed\n"
	terminal := "wcslib_wtbarr_wrap.c:209:3: error: incompatible function pointer types\n2 errors generated\n"
	diagnostic := prefix + strings.Repeat("compiling café source unit\n", 4_000) + terminal
	response := &runtimev0.TestResponse{Result: &runtimev0.TestRunResult{
		State:   runtimev0.TestRunResult_ERRORED,
		Message: diagnostic,
	}}

	got := testResponseLogSummary(response)
	if len(got) > maxTestLogMessageBytes {
		t.Fatalf("live summary bytes = %d, want <= %d", len(got), maxTestLogMessageBytes)
	}
	for _, want := range []string{prefix, "bytes omitted from live log", strings.TrimSpace(terminal)} {
		if !strings.Contains(got, want) {
			t.Fatalf("bounded summary omitted %q", want)
		}
	}
	if !utf8.ValidString(got) {
		t.Fatal("bounded summary split a UTF-8 sequence")
	}
	if response.GetResult().GetMessage() != diagnostic {
		t.Fatal("live log projection mutated the typed TestResponse diagnostic")
	}
}

// TestEnrichSuppliedProvisioning locks the supplied-formula environment
// contract: the caller owns WHAT to run, the plugin derives the uv
// environment around it (editable install, interpreter pin), and explicitly
// supplied keys always win. This is the regression test for the SWE-bench
// easy-tier failure where django's supplied "python runtests.py" formula ran
// without --with-editable . and env-blocked with "Django module not found".
func TestEnrichSuppliedProvisioning(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte("from setuptools import setup\nsetup(name='pkg')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".python-version"), []byte("3.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("empty bag gains derived environment", func(t *testing.T) {
		got := enrichSuppliedProvisioning(nil, dir)
		if got["editable"] != "true" {
			t.Fatalf("editable = %q, want true (project package must be importable)", got["editable"])
		}
		if got["no_project"] != "true" {
			t.Fatalf("no_project = %q, want true", got["no_project"])
		}
		if got["python"] != "3.9" {
			t.Fatalf("python = %q, want 3.9 from .python-version", got["python"])
		}
	})

	t.Run("supplied keys win over derived", func(t *testing.T) {
		got := enrichSuppliedProvisioning(map[string]string{"editable": "false", "python": "3.11"}, dir)
		if got["editable"] != "false" {
			t.Fatalf("editable = %q, want supplied false to win", got["editable"])
		}
		if got["python"] != "3.11" {
			t.Fatalf("python = %q, want supplied 3.11 to win", got["python"])
		}
		if got["no_project"] != "true" {
			t.Fatalf("no_project = %q, want derived true to fill the gap", got["no_project"])
		}
	})

	t.Run("no source dir passes supplied through", func(t *testing.T) {
		supplied := map[string]string{"editable": "false"}
		got := enrichSuppliedProvisioning(supplied, "")
		if len(got) != 1 || got["editable"] != "false" {
			t.Fatalf("got %v, want supplied bag unchanged", got)
		}
	})
}

// A per-call formula with NO command but WITH provisioning/env — a Mind
// environment heal such as {cwd: tests} or {with: Werkzeug<3} — must OVERLAY
// the configured/derived formula instead of being dropped. This locks the
// remote-agent path against the django-trace defect where a healed work_dir
// sent without a command was a silent no-op (mirrors Mind's InProcessRuntime).
func TestResolveTestFormulaOverlaysCommandlessHealOntoConfiguredFormula(t *testing.T) {
	svc := &resources.Service{
		Test: &resources.TestFormula{
			Command:      []string{"python", "runtests.py"},
			Output:       "unittest-text",
			Provisioning: map[string]string{"python": "3.9"},
			Env:          map[string]string{"KEEP": "1"},
		},
	}
	req := &runtimev0.TestRequest{Formula: &runtimev0.TestFormula{
		Provisioning: map[string]string{"cwd": "tests", "with": "Werkzeug<3"},
		Env:          map[string]string{"PYTHONPATH": "."},
	}}

	cmd, output, env, prov, ok := resolveTestFormula(req, svc, "")
	if !ok {
		t.Fatal("resolveTestFormula returned ok=false")
	}
	if len(cmd) != 2 || cmd[0] != "python" || cmd[1] != "runtests.py" {
		t.Fatalf("command = %v, want the configured command", cmd)
	}
	if output != "unittest-text" {
		t.Fatalf("output = %q", output)
	}
	if prov["cwd"] != "tests" || prov["with"] != "Werkzeug<3" {
		t.Fatalf("healed provisioning must overlay the configured formula, got %v", prov)
	}
	if prov["python"] != "3.9" {
		t.Fatalf("configured provisioning must survive the overlay, got %v", prov)
	}
	if env["PYTHONPATH"] != "." || env["KEEP"] != "1" {
		t.Fatalf("env overlay wrong: %v", env)
	}
}

func TestResolveTestFormulaOverlaysCommandlessServiceConfigurationOntoDerivedFormula(t *testing.T) {
	dir := t.TempDir()
	workflow := filepath.Join(dir, ".github", "workflows", "test.yml")
	if err := os.MkdirAll(filepath.Dir(workflow), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflow, []byte("jobs:\n  test:\n    steps:\n      - run: python -m unittest -v test_environment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := &resources.Service{Test: &resources.TestFormula{
		Env:          map[string]string{"RECOVERY_FLAG": "enabled"},
		Provisioning: map[string]string{"python": "3.11"},
	}}

	cmd, output, env, provisioning, ok := resolveTestFormula(&runtimev0.TestRequest{}, svc, dir)
	if !ok {
		t.Fatal("resolveTestFormula returned ok=false")
	}
	if got := strings.Join(cmd, " "); got != "python -m unittest -v test_environment" {
		t.Fatalf("derived command = %q", got)
	}
	if output != "unittest-text" {
		t.Fatalf("output = %q", output)
	}
	if env["RECOVERY_FLAG"] != "enabled" || provisioning["python"] != "3.11" {
		t.Fatalf("service recovery overlay missing: env=%v provisioning=%v", env, provisioning)
	}
}

func TestResolveTestFormulaOverlaysCommandlessHealOntoDerivedFormula(t *testing.T) {
	dir := t.TempDir()
	toxIni := "[tox]\nenvlist = py\n\n[testenv]\ncommands =\n    python -m unittest -v test_sample {posargs}\n"
	if err := os.WriteFile(filepath.Join(dir, "tox.ini"), []byte(toxIni), 0o644); err != nil {
		t.Fatal(err)
	}
	req := &runtimev0.TestRequest{Formula: &runtimev0.TestFormula{
		Provisioning: map[string]string{"cwd": "tests"},
	}}

	cmd, _, _, prov, ok := resolveTestFormula(req, nil, dir)
	if !ok {
		t.Fatal("resolveTestFormula returned ok=false (derivation should succeed from tox.ini)")
	}
	if len(cmd) == 0 || cmd[0] != "python" {
		t.Fatalf("command = %v, want the tox-derived command", cmd)
	}
	if prov["cwd"] != "tests" {
		t.Fatalf("healed cwd must overlay the DERIVED formula, got %v", prov)
	}
	if prov["no_project"] != "true" {
		t.Fatalf("derived provisioning must survive the overlay, got %v", prov)
	}
}
