// Package code implements the generic Python Code gRPC service.
// Specializations (python-fastapi, python-grpc, …) can embed *Code and
// override or add Python-specific handlers.
package code

import (
	"context"
	"os"

	corecode "github.com/codefly-dev/core/code"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"

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
// explicit Service.SourceLocation → CODEFLY_AGENT_WORKDIR → Service.Location.
func (c *Code) SourceDir() string {
	if c.Service.SourceLocation != "" {
		return c.Service.SourceLocation
	}
	if wd := os.Getenv("CODEFLY_AGENT_WORKDIR"); wd != "" {
		return wd
	}
	return c.Service.Location
}

// EnsureInit lazily swaps in a PythonCodeServer pointed at the resolved
// source directory the first time an RPC lands. Exported so specializations
// calling embedded RPC methods can share the same guard.
func (c *Code) EnsureInit() {
	if !c.initialized {
		c.PythonCodeServer = corecode.NewPythonCodeServer(c.SourceDir(), nil)
		c.initialized = true
	}
}

// ── Lazy-init RPC wrappers ───────────────────────────────

func (c *Code) GetProjectInfo(ctx context.Context, req *codev0.GetProjectInfoRequest) (*codev0.GetProjectInfoResponse, error) {
	c.EnsureInit()
	return c.PythonCodeServer.GetProjectInfo(ctx, req)
}

func (c *Code) ListDependencies(ctx context.Context, req *codev0.ListDependenciesRequest) (*codev0.ListDependenciesResponse, error) {
	c.EnsureInit()
	return c.PythonCodeServer.ListDependencies(ctx, req)
}

func (c *Code) Execute(ctx context.Context, req *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	c.EnsureInit()
	return c.PythonCodeServer.Execute(ctx, req)
}
