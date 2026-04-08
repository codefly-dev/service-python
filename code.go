package main

import (
	"context"
	"os"

	corecode "github.com/codefly-dev/core/code"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
)

type Code struct {
	*corecode.PythonCodeServer
	*Service
	initialized bool
}

func NewCode(svc *Service) *Code {
	c := &Code{
		Service:          svc,
		PythonCodeServer: corecode.NewPythonCodeServer(".", nil),
	}
	c.registerOverrides()
	return c
}

func (c *Code) sourceDir() string {
	if c.sourceLocation != "" {
		return c.sourceLocation
	}
	if wd := os.Getenv("CODEFLY_AGENT_WORKDIR"); wd != "" {
		return wd
	}
	return c.Location
}

func (c *Code) ensureInit() {
	if !c.initialized {
		c.PythonCodeServer = corecode.NewPythonCodeServer(c.sourceDir(), nil)
		c.registerOverrides() // re-register after re-init
		c.initialized = true
	}
}

// registerOverrides wires Python-specific handlers on top of PythonCodeServer.
// PythonCodeServer already provides: list_symbols (AST), get_project_info, list_dependencies.
// We add: get_call_graph (AST-based) and find_references (grep-based).
func (c *Code) registerOverrides() {
	c.Override("get_call_graph", c.handleGetCallGraph)
	c.Override("find_references", c.handleFindReferences)
}

// Lazy init wrappers for standalone gRPC RPCs.

func (c *Code) GetProjectInfo(ctx context.Context, req *codev0.GetProjectInfoRequest) (*codev0.GetProjectInfoResponse, error) {
	c.ensureInit()
	return c.PythonCodeServer.GetProjectInfo(ctx, req)
}

func (c *Code) ListSymbols(ctx context.Context, req *codev0.ListSymbolsRequest) (*codev0.ListSymbolsResponse, error) {
	c.ensureInit()
	return c.PythonCodeServer.ListSymbols(ctx, req)
}

func (c *Code) ListDependencies(ctx context.Context, req *codev0.ListDependenciesRequest) (*codev0.ListDependenciesResponse, error) {
	c.ensureInit()
	return c.PythonCodeServer.ListDependencies(ctx, req)
}

func (c *Code) Execute(ctx context.Context, req *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	c.ensureInit()
	return c.PythonCodeServer.Execute(ctx, req)
}
