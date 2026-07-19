package code_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	"github.com/codefly-dev/core/resources"
	pythoncode "github.com/codefly-dev/service-python/pkg/code"
	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

func TestFixUsesRuffAndHonorsDryRun(t *testing.T) {
	if _, err := exec.LookPath("ruff"); err != nil {
		t.Skip("ruff is not installed")
	}
	dir := t.TempDir()
	original := "import os\n\ndef answer( ):\n return 42\n"
	if err := os.WriteFile(filepath.Join(dir, "sample.py"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	svc.SourceLocation = dir
	server := pythoncode.New(svc)
	response, err := server.Execute(context.Background(), &codev0.CodeRequest{Operation: &codev0.CodeRequest_Fix{Fix: &codev0.FixRequest{
		File: "sample.py", DryRun: true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	fix := response.GetFix()
	if !fix.GetSuccess() || !fix.GetChanged() || fix.GetWrote() {
		t.Fatalf("dry-run fix = %+v failure=%+v", fix, response.GetFailure())
	}
	if strings.Contains(fix.GetContent(), "import os") || !strings.Contains(fix.GetContent(), "def answer():") {
		t.Fatalf("Ruff pipeline did not lint+format:\n%s", fix.GetContent())
	}
	written, err := os.ReadFile(filepath.Join(dir, "sample.py"))
	if err != nil || string(written) != original {
		t.Fatalf("dry-run changed source: err=%v content=%q", err, written)
	}
	for _, unexpected := range []string{".venv", "uv.lock"} {
		if _, err := os.Stat(filepath.Join(dir, unexpected)); !os.IsNotExist(err) {
			t.Fatalf("dry-run materialized %s: %v", unexpected, err)
		}
	}
}

func TestApplyEditRunsRuffSafeFixerByDefault(t *testing.T) {
	if _, err := exec.LookPath("ruff"); err != nil {
		t.Skip("ruff is not installed")
	}
	dir := t.TempDir()
	original := "import os\n\ndef answer( ):\n return 42\n"
	if err := os.WriteFile(filepath.Join(dir, "sample.py"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	svc.SourceLocation = dir
	server := pythoncode.New(svc)
	response, err := server.Execute(context.Background(), &codev0.CodeRequest{Operation: &codev0.CodeRequest_ApplyEdit{ApplyEdit: &codev0.ApplyEditRequest{
		File: "sample.py", Find: "return 42", Replace: "return( 43 )",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	edit := response.GetApplyEdit()
	if !edit.GetSuccess() || !edit.GetChanged() || !edit.GetWrote() {
		t.Fatalf("ApplyEdit = %+v failure=%+v", edit, response.GetFailure())
	}
	if strings.Contains(edit.GetContent(), "import os") || !strings.Contains(edit.GetContent(), "def answer():\n    return 43") {
		t.Fatalf("Ruff was not composed into ApplyEdit:\n%s", edit.GetContent())
	}
	written, err := os.ReadFile(filepath.Join(dir, "sample.py"))
	if err != nil || string(written) != edit.GetContent() {
		t.Fatalf("ApplyEdit did not commit returned content: err=%v content=%q", err, written)
	}
}
