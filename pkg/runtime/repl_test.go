package runtime_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/service-python/pkg/runtime"
)

// TestPythonRepl_StatePersistence is the core guarantee: variables and
// imports defined in one Exec are visible to the next. Without this the
// "persistent REPL" claim is meaningless.
func TestPythonRepl_StatePersistence(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	ctx := context.Background()
	env, err := runners.NewNativeEnvironment(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	repl, err := runtime.NewPythonRepl(ctx, env)
	if err != nil {
		t.Fatalf("NewPythonRepl: %v", err)
	}
	t.Cleanup(func() { _ = repl.Shutdown() })

	// Exec 1: define a variable. Output should be empty (no print).
	out, err := repl.Exec(ctx, "x = 42")
	if err != nil {
		t.Fatalf("Exec 1: %v (out=%q)", err, out)
	}

	// Exec 2: import something. Import state must persist.
	out, err = repl.Exec(ctx, "import math\ny = math.sqrt(x + 7)")
	if err != nil {
		t.Fatalf("Exec 2: %v (out=%q)", err, out)
	}

	// Exec 3: use both the variable and the import together. The output
	// is what proves state actually crossed call boundaries.
	out, err = repl.Exec(ctx, "print(f\"{x} -> {y:.4f}\")")
	if err != nil {
		t.Fatalf("Exec 3: %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "42 -> 7.0000") {
		t.Fatalf("expected state-persistence output, got %q", out)
	}
}

// TestPythonRepl_ErrorSurfaced asserts that a raised exception in user
// code is reported as an error — not swallowed silently into "OK".
func TestPythonRepl_ErrorSurfaced(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	ctx := context.Background()
	env, err := runners.NewNativeEnvironment(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	repl, err := runtime.NewPythonRepl(ctx, env)
	if err != nil {
		t.Fatalf("NewPythonRepl: %v", err)
	}
	t.Cleanup(func() { _ = repl.Shutdown() })

	out, err := repl.Exec(ctx, "raise ValueError('boom')")
	if err == nil {
		t.Fatalf("expected error from raising ValueError, got nil (out=%q)", out)
	}
	if !strings.Contains(out, "ValueError") || !strings.Contains(out, "boom") {
		t.Fatalf("expected traceback with ValueError/boom in output, got %q", out)
	}

	// Critical: the REPL must STILL BE ALIVE after an error. The next
	// exec should work normally.
	out, err = repl.Exec(ctx, "print('alive')")
	if err != nil {
		t.Fatalf("post-error Exec: %v", err)
	}
	if !strings.Contains(out, "alive") {
		t.Fatalf("expected 'alive', got %q", out)
	}
}

// TestPythonRepl_ResetClearsState exercises the repl-reset command's
// underlying Shutdown + boot-fresh flow by Shutting down and creating
// a new instance, verifying variables from the old session are gone.
func TestPythonRepl_ResetClearsState(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	ctx := context.Background()
	env, err := runners.NewNativeEnvironment(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("env: %v", err)
	}

	first, err := runtime.NewPythonRepl(ctx, env)
	if err != nil {
		t.Fatalf("first NewPythonRepl: %v", err)
	}
	if _, err := first.Exec(ctx, "secret = 'hello'"); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	_ = first.Shutdown()

	// Fresh REPL: `secret` must be undefined → NameError.
	second, err := runtime.NewPythonRepl(ctx, env)
	if err != nil {
		t.Fatalf("second NewPythonRepl: %v", err)
	}
	t.Cleanup(func() { _ = second.Shutdown() })

	out, err := second.Exec(ctx, "print(secret)")
	if err == nil {
		t.Fatalf("expected NameError in fresh REPL, got nil (out=%q)", out)
	}
	if !strings.Contains(out, "NameError") {
		t.Fatalf("expected NameError traceback, got %q", out)
	}
}
