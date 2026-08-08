package runtime_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	selectioncontract "github.com/codefly-dev/core/runners/testselection"

	pythonruntime "github.com/codefly-dev/service-python/pkg/runtime"
	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

// TestRuntimeEmbedsService verifies the embedding chain:
//
//	runtime.Runtime → *service.Service → *services.Base
//
// Specializations rely on this chain to inherit Wool, Logger, Location,
// Identity, etc. via method promotion. If embedding is replaced with a
// named field this test breaks loudly.
func TestRuntimeEmbedsService(t *testing.T) {
	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	rt := pythonruntime.New(svc)

	if rt == nil {
		t.Fatal("New returned nil")
	}
	if rt.Service != svc {
		t.Error("embedded Service is not the same pointer passed to New")
	}
	// Promoted fields from *services.Base must be reachable on *Runtime.
	// If these compile, the chain is intact; no runtime assertion needed.
	_ = rt.Base     // from services.Base promoted through *pythonservice.Service
	_ = rt.Settings // from pythonservice.Service
	_ = rt.Runtime  // from services.RuntimeServer
}

func TestRuntimeInitDoesNotMutateSourceCheckout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname = \"read-only-init\"\nversion = \"0.1.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	svc.SourceLocation = dir
	rt := pythonruntime.New(svc)
	if _, err := rt.Init(context.Background(), &runtimev0.InitRequest{}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	for _, generated := range []string{"uv.lock", ".venv"} {
		if _, err := os.Stat(filepath.Join(dir, generated)); !os.IsNotExist(err) {
			t.Fatalf("Init generated %s in source checkout", generated)
		}
	}
}

// TestRuntimeHonorsTypedSelectionWithDefaultRunner locks the remote Python
// agent to the same contract as Codefly's local library: a markerless Python
// workspace uses the real default runner, runs only the selected case, and
// returns the structured acknowledgement even though the selected test fails.
func TestRuntimeHonorsTypedSelectionWithDefaultRunner(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Fatalf("uv is required for the production Python runtime: %v", err)
	}
	dir := t.TempDir()
	content := "import unittest\n\n\nclass CalculatorTests(unittest.TestCase):\n    def test_selected_failure(self):\n        self.assertEqual(1, 2)\n\n    def test_unselected_pass(self):\n        self.assertEqual(2, 2)\n"
	if err := os.WriteFile(filepath.Join(dir, "test_calc.py"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	svc.SourceLocation = dir
	rt := pythonruntime.New(svc)
	req := &runtimev0.TestRequest{
		Selection: &runtimev0.TestSelection{Scope: &runtimev0.TestSelection_TestCase{TestCase: &runtimev0.TestCaseSelection{
			Path:          "test_calc.py",
			QualifiedName: []string{"CalculatorTests", "test_selected_failure"},
		}}},
		SelectionId: "markerless-python-selected-case",
	}
	resp, err := rt.Test(context.Background(), req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if err := selectioncontract.VerifyAcknowledgement(req, resp); err != nil {
		t.Fatalf("selection acknowledgement: %v", err)
	}
	if resp.GetResult().GetState() != runtimev0.TestRunResult_FAILED || resp.GetCounts().GetTotal() != 1 || resp.GetCounts().GetFailed() != 1 {
		t.Fatalf("selected result = %s counts=%+v, want only one failing case", resp.GetResult().GetState(), resp.GetCounts())
	}
	assertRuntimeTestLeftSourceClean(t, dir)
}

// TestRuntimeDefaultRunnerMaterializesDeclaredRequirements proves the remote
// agent surface, not only Core's helper. The imported package is available
// exclusively through requirements.txt, so a pytest-only uv overlay must fail
// and the project-derived provisioning contract must pass.
func TestRuntimeDefaultRunnerMaterializesDeclaredRequirements(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Fatalf("uv is required for the production Python runtime: %v", err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "supportdep"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("supportdep/pyproject.toml", `[build-system]
requires = ["setuptools>=68"]
build-backend = "setuptools.build_meta"

[project]
name = "codefly-agent-declared-probe-dependency"
version = "0.0.1"

[tool.setuptools]
py-modules = ["agent_declared_probe_dependency"]
`)
	write("supportdep/agent_declared_probe_dependency.py", "VALUE = 'from-agent-declared-requirements'\n")
	write("requirements.txt", "./supportdep\n")
	write("test_declared_dependency.py", `import agent_declared_probe_dependency

def test_dependency_was_materialized_from_project_declaration():
    assert agent_declared_probe_dependency.VALUE == "from-agent-declared-requirements"
`)

	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	svc.SourceLocation = root
	rt := pythonruntime.New(svc)
	resp, err := rt.Test(context.Background(), &runtimev0.TestRequest{})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.GetResult().GetState() != runtimev0.TestRunResult_PASSED || resp.GetCounts().GetTotal() != 1 || resp.GetCounts().GetPassed() != 1 {
		t.Fatalf("result = %s counts=%+v message=%q, want one passed test", resp.GetResult().GetState(), resp.GetCounts(), resp.GetResult().GetMessage())
	}
	assertRuntimeTestLeftSourceClean(t, root)
}

// assertRuntimeTestLeftSourceClean proves the production agent's default Test
// RPC is observational even when uv invokes a packaging backend for the root
// project or a local declared dependency.
func assertRuntimeTestLeftSourceClean(t *testing.T, root string) {
	t.Helper()
	for _, generated := range []string{"uv.lock", ".venv", ".pytest_cache", "__pycache__", ".cache"} {
		if _, err := os.Stat(filepath.Join(root, generated)); !os.IsNotExist(err) {
			t.Fatalf("production agent generated %s in source checkout", generated)
		}
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".egg-info") {
			t.Fatalf("production agent generated package metadata in source checkout: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspect source checkout after agent test: %v", err)
	}
}
