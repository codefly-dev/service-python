// Package tooling implements the Tooling gRPC service for the generic
// Python agent. It delegates to Code for edits/project metadata and Runtime
// for test/lint. Specializations typically do NOT override Tooling — their
// Code/Runtime overrides flow through automatically.
package tooling

import (
	"context"
	"fmt"

	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	toolingv0 "github.com/codefly-dev/core/generated/go/codefly/services/tooling/v0"

	pythoncode "github.com/codefly-dev/service-python/pkg/code"
	pythonruntime "github.com/codefly-dev/service-python/pkg/runtime"
)

// Tooling implements the Tooling gRPC service for Python.
//
// ARCHITECTURE: Tooling is the primary interface Mind calls for
// language-specific commands. It unifies Code and Runtime:
//   - Analysis: GetProjectInfo (via Code)
//   - Modification: Fix (ruff), ApplyEdit (via Code)
//   - Dev: Build, Test (pytest), Lint (ruff) (delegates to Runtime)
type Tooling struct {
	toolingv0.UnimplementedToolingServer
	Code    *pythoncode.Code
	Runtime *pythonruntime.Runtime
}

// New builds a Tooling server wired to the given Code and Runtime.
func New(code *pythoncode.Code, rt *pythonruntime.Runtime) *Tooling {
	return &Tooling{Code: code, Runtime: rt}
}

// ── Code Modification ──────────────────────────────────

func (t *Tooling) Fix(ctx context.Context, req *toolingv0.FixRequest) (*toolingv0.FixResponse, error) {
	resp, err := t.Code.Execute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_Fix{Fix: &codev0.FixRequest{File: req.File}},
	})
	if err != nil {
		return nil, fmt.Errorf("tooling fix: %w", err)
	}
	fix := resp.GetFix()
	if fix == nil {
		return &toolingv0.FixResponse{Success: false, Error: "no response"}, nil
	}
	return &toolingv0.FixResponse{
		Success: fix.Success, Content: fix.Content, Error: fix.Error, Actions: fix.Actions,
	}, nil
}

func (t *Tooling) ApplyEdit(ctx context.Context, req *toolingv0.ApplyEditRequest) (*toolingv0.ApplyEditResponse, error) {
	resp, err := t.Code.Execute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_ApplyEdit{ApplyEdit: &codev0.ApplyEditRequest{
			File: req.File, Find: req.Find, Replace: req.Replace, AutoFix: req.AutoFix,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("tooling apply_edit: %w", err)
	}
	ae := resp.GetApplyEdit()
	if ae == nil {
		return &toolingv0.ApplyEditResponse{Success: false, Error: "no response"}, nil
	}
	return &toolingv0.ApplyEditResponse{
		Success: ae.Success, Content: ae.Content,
		Error: ae.Error, Strategy: ae.Strategy, FixActions: ae.FixActions,
	}, nil
}

// ── Dependencies ───────────────────────────────────────

func (t *Tooling) ListDependencies(ctx context.Context, _ *toolingv0.ListDependenciesRequest) (*toolingv0.ListDependenciesResponse, error) {
	resp, err := t.Code.Execute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_ListDependencies{ListDependencies: &codev0.ListDependenciesRequest{}},
	})
	if err != nil {
		return nil, fmt.Errorf("tooling list_dependencies: %w", err)
	}
	ld := resp.GetListDependencies()
	if ld == nil {
		return &toolingv0.ListDependenciesResponse{}, nil
	}
	var deps []*toolingv0.Dependency
	for _, d := range ld.Dependencies {
		deps = append(deps, &toolingv0.Dependency{Name: d.Name, Version: d.Version, Direct: d.Direct})
	}
	return &toolingv0.ListDependenciesResponse{Dependencies: deps, Error: ld.Error}, nil
}

func (t *Tooling) AddDependency(ctx context.Context, req *toolingv0.AddDependencyRequest) (*toolingv0.AddDependencyResponse, error) {
	resp, err := t.Code.Execute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_AddDependency{AddDependency: &codev0.AddDependencyRequest{
			PackageName: req.PackageName, Version: req.Version,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("tooling add_dependency: %w", err)
	}
	ad := resp.GetAddDependency()
	if ad == nil {
		return &toolingv0.AddDependencyResponse{Success: false, Error: "no response"}, nil
	}
	return &toolingv0.AddDependencyResponse{
		Success: ad.Success, Error: ad.Error, InstalledVersion: ad.InstalledVersion,
	}, nil
}

func (t *Tooling) RemoveDependency(ctx context.Context, req *toolingv0.RemoveDependencyRequest) (*toolingv0.RemoveDependencyResponse, error) {
	resp, err := t.Code.Execute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_RemoveDependency{RemoveDependency: &codev0.RemoveDependencyRequest{
			PackageName: req.PackageName,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("tooling remove_dependency: %w", err)
	}
	rd := resp.GetRemoveDependency()
	if rd == nil {
		return &toolingv0.RemoveDependencyResponse{Success: false, Error: "no response"}, nil
	}
	return &toolingv0.RemoveDependencyResponse{Success: rd.Success, Error: rd.Error}, nil
}

// ── Analysis ───────────────────────────────────────────

func (t *Tooling) GetProjectInfo(ctx context.Context, _ *toolingv0.GetProjectInfoRequest) (*toolingv0.GetProjectInfoResponse, error) {
	resp, err := t.Code.Execute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GetProjectInfo{GetProjectInfo: &codev0.GetProjectInfoRequest{}},
	})
	if err != nil {
		return nil, fmt.Errorf("tooling get_project_info: %w", err)
	}
	pi := resp.GetGetProjectInfo()
	if pi == nil {
		return &toolingv0.GetProjectInfoResponse{}, nil
	}
	var pkgs []*toolingv0.PackageInfo
	for _, p := range pi.Packages {
		pkgs = append(pkgs, &toolingv0.PackageInfo{
			Name: p.Name, RelativePath: p.RelativePath,
			Files: p.Files, Imports: p.Imports, Doc: p.Doc,
		})
	}
	var deps []*toolingv0.Dependency
	for _, d := range pi.Dependencies {
		deps = append(deps, &toolingv0.Dependency{Name: d.Name, Version: d.Version, Direct: d.Direct})
	}
	return &toolingv0.GetProjectInfoResponse{
		Module: pi.Module, Language: pi.Language, LanguageVersion: pi.LanguageVersion,
		Packages: pkgs, Dependencies: deps, FileHashes: pi.FileHashes, Error: pi.Error,
	}, nil
}

// ── Dev Validation (delegates to Runtime) ──────────────

func (t *Tooling) Build(_ context.Context, _ *toolingv0.BuildRequest) (*toolingv0.BuildResponse, error) {
	return &toolingv0.BuildResponse{Success: true, Output: "no build needed (Python)"}, nil
}

func (t *Tooling) Test(ctx context.Context, _ *toolingv0.TestRequest) (*toolingv0.TestResponse, error) {
	// Pass a non-nil request: Runtime.Test dereferences req.Target, so nil
	// panicked the agent (and Wool.Catch then nil-deref'd resp.Status).
	resp, err := t.Runtime.Test(ctx, &runtimev0.TestRequest{})
	if err != nil {
		return nil, fmt.Errorf("tooling test: %w", err)
	}
	success := resp.Status != nil && resp.Status.State == runtimev0.TestStatus_SUCCESS
	return &toolingv0.TestResponse{
		Success: success, Output: resp.Output,
		TestsRun: resp.TestsRun, TestsPassed: resp.TestsPassed,
		TestsFailed: resp.TestsFailed, TestsSkipped: resp.TestsSkipped,
		CoveragePct: resp.CoveragePct, Failures: resp.Failures,
	}, nil
}

func (t *Tooling) Lint(ctx context.Context, _ *toolingv0.LintRequest) (*toolingv0.LintResponse, error) {
	resp, err := t.Runtime.Lint(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("tooling lint: %w", err)
	}
	success := resp.Status != nil && resp.Status.State == runtimev0.LintStatus_SUCCESS
	return &toolingv0.LintResponse{Success: success, Output: resp.Output}, nil
}
