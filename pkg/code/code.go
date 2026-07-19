// Package code implements the generic Python Code gRPC service.
// Specializations (python-fastapi, python-grpc, …) can embed *Code and
// override or add Python-specific handlers.
package code

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corecode "github.com/codefly-dev/core/code"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runners "github.com/codefly-dev/core/runners/base"

	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

// Code implements the Code gRPC service for Python.
// It embeds PythonCodeServer from core, which provides:
//   - File ops: ReadFile, WriteFile, CreateFile, DeleteFile, MoveFile, ListFiles, Search
//   - Git ops: GitLog, GitDiff, GitShow, GitBlame
//   - Analysis: GetProjectInfo, ListDependencies
//   - Smart edit: ApplyEdit (fuzzy find/replace)
type Code struct {
	*corecode.PythonCodeServer
	Service *pythonservice.Service

	initialized bool
}

// New builds a generic Python Code server bound to the shared Service.
// The PythonCodeServer is re-initialized on first RPC with the resolved
// sourceDir (which isn't known until Runtime.Load has run).
func New(svc *pythonservice.Service) *Code {
	c := &Code{
		Service:          svc,
		PythonCodeServer: corecode.NewPythonCodeServer(".", nil),
	}
	return c
}

// SourceDir returns the directory to operate on. Resolution order:
// explicit Service.SourceLocation → CODEFLY_AGENT_WORKDIR/source-dir → Service.Location/source-dir.
func (c *Code) SourceDir() string {
	if c.Service.SourceLocation != "" {
		return c.Service.SourceLocation
	}
	if wd := os.Getenv("CODEFLY_AGENT_WORKDIR"); wd != "" {
		return filepath.Join(wd, c.Service.Settings.PythonSourceDir())
	}
	return filepath.Join(c.Service.Location, c.Service.Settings.PythonSourceDir())
}

// EnsureInit lazily swaps in a PythonCodeServer pointed at the resolved
// source directory the first time an RPC lands. Exported so specializations
// calling embedded RPC methods can share the same guard.
func (c *Code) EnsureInit() {
	if !c.initialized {
		c.PythonCodeServer = corecode.NewPythonCodeServer(c.SourceDir(), nil)
		c.SetSourceFixer(c.fixPython)
		c.initialized = true
	}
}

func (c *Code) Execute(ctx context.Context, req *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	c.EnsureInit()
	return c.PythonCodeServer.Execute(ctx, req)
}

// fixPython applies only project-configured Ruff safe fixes, then Ruff's
// formatter. Both stages operate on stdin and return source on stdout, so the
// core Code server remains the sole writer and can honor VFS/dry-run semantics.
func (c *Code) fixPython(ctx context.Context, input corecode.FixInput) (corecode.FixResult, error) {
	checkArgs := []string{"check", "--fix-only"}
	if input.Mode == basev0.FixMode_FIX_MODE_AGGRESSIVE {
		checkArgs = append(checkArgs, "--unsafe-fixes")
	}
	checkArgs = append(checkArgs, "--stdin-filename", input.Path, "-")

	// Fix is an authoring primitive, not environment initialization. Invoke a
	// provisioned Ruff directly so a dry run cannot create .venv/uv.lock or
	// download the project's dependency graph. Factory Nix shells include Ruff,
	// the Docker runtime exposes /venv/bin on PATH, and native users may supply
	// either a project-local .venv binary or Ruff on the host PATH.
	ruff := c.ruffCommand()
	checked, checkLogBytes, err := runners.RunInput(ctx, c.runnerEnvironment(ctx), c.SourceDir(), input.Content, ruff, checkArgs...)
	checkLog := string(checkLogBytes)
	if err != nil {
		return corecode.FixResult{}, fmt.Errorf("ruff check --fix-only (Ruff must already be provisioned): %w: %s", err, strings.TrimSpace(checkLog))
	}
	formatted, formatLogBytes, err := runners.RunInput(ctx, c.runnerEnvironment(ctx), c.SourceDir(), checked, ruff, "format", "--stdin-filename", input.Path, "-")
	formatLog := string(formatLogBytes)
	if err != nil {
		return corecode.FixResult{}, fmt.Errorf("ruff format: %w: %s", err, strings.TrimSpace(formatLog))
	}
	output := strings.TrimSpace(strings.Join([]string{checkLog, formatLog}, "\n"))
	return corecode.FixResult{
		Content: formatted,
		Actions: []string{"ruff check --fix-only", "ruff format"},
		Output:  output,
	}, nil
}

func (c *Code) ruffCommand() string {
	if c.Service.ActiveEnv == nil {
		candidate := filepath.Join(c.SourceDir(), ".venv", "bin", "ruff")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return "ruff"
}

func (c *Code) runnerEnvironment(ctx context.Context) runners.RunnerEnvironment {
	env := c.Service.ActiveEnv
	if env != nil {
		return env
	}
	var runtimeContext *basev0.RuntimeContext
	if c.Service.Base != nil && c.Service.Base.Runtime != nil {
		runtimeContext = c.Service.Base.Runtime.RuntimeContext
	}
	return runners.ResolveStandaloneEnvironment(ctx, c.SourceDir(), runtimeContext)
}
