package code_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
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

// TestCodeIsSafeWhileTheActiveEnvironmentIsRepublished drives the two Code
// entry points that resolve a runner environment — Fix (Ruff) and the call
// graph (AST) — while a specialization publishes its environment from Init
// and clears it from Stop. The RPCs run on one goroutine so the only thing
// racing the publisher is the environment lookup itself.
//
// Both requests must keep answering: a Fix that lands between publish and
// clear falls back to a standalone environment rather than failing.
func TestCodeIsSafeWhileTheActiveEnvironmentIsRepublished(t *testing.T) {
	if _, err := exec.LookPath("ruff"); err != nil {
		t.Skip("ruff is not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.py"), []byte("def answer( ):\n return 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	svc.SourceLocation = dir
	env, err := runners.NewNativeEnvironment(ctx, dir)
	if err != nil {
		t.Fatalf("native environment: %v", err)
	}
	server := pythoncode.New(svc)

	done := make(chan struct{})
	var publisher sync.WaitGroup
	publisher.Add(1)
	go func() {
		defer publisher.Done()
		for {
			select {
			case <-done:
				return
			default:
				svc.SetActiveEnvironment(env)
				svc.SetActiveEnvironment(nil)
			}
		}
	}()

	for i := 0; i < 10; i++ {
		response, err := server.Execute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_Fix{Fix: &codev0.FixRequest{
			File: "sample.py", DryRun: true,
		}}})
		if err != nil {
			t.Fatalf("fix %d: %v", i, err)
		}
		if fix := response.GetFix(); !fix.GetSuccess() {
			t.Fatalf("fix %d did not run: %+v failure=%+v", i, fix, response.GetFailure())
		}
		if graph := server.ComputePythonCallGraph(ctx, dir); graph.Error != "" {
			t.Fatalf("call graph %d: %s", i, graph.Error)
		}
	}
	close(done)
	publisher.Wait()
}
