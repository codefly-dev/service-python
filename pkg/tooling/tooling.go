// Package tooling implements the Tooling gRPC service for the generic
// Python agent. It delegates to Code for edits/project metadata and Runtime
// for test/lint. Specializations typically do NOT override Tooling — their
// Code/Runtime overrides flow through automatically.
package tooling

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/failures"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
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
		Operation: &codev0.CodeRequest_Fix{Fix: &codev0.FixRequest{File: req.GetFile(), Mode: req.GetMode(), DryRun: req.GetDryRun()}},
	})
	if err != nil {
		return nil, fmt.Errorf("tooling fix: %w", err)
	}
	fix := resp.GetFix()
	if fix == nil {
		return &toolingv0.FixResponse{Success: false, Failure: failures.Ensure(resp.GetFailure(), basev0.FailureCode_FAILURE_CODE_INTERNAL, "tooling.fix", "code service returned no fix result")}, nil
	}
	return &toolingv0.FixResponse{
		Success: fix.Success, Content: fix.Content, Actions: fix.Actions, Failure: failures.Clone(resp.GetFailure()),
		Changed: fix.Changed, BeforeSha256: fix.BeforeSha256, AfterSha256: fix.AfterSha256,
		Wrote: fix.Wrote, Output: fix.Output,
	}, nil
}

func (t *Tooling) ApplyEdit(ctx context.Context, req *toolingv0.ApplyEditRequest) (*toolingv0.ApplyEditResponse, error) {
	resp, err := t.Code.Execute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_ApplyEdit{ApplyEdit: &codev0.ApplyEditRequest{
			File: req.GetFile(), Find: req.GetFind(), Replace: req.GetReplace(), FixMode: req.GetFixMode(), DryRun: req.GetDryRun(),
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("tooling apply_edit: %w", err)
	}
	ae := resp.GetApplyEdit()
	if ae == nil {
		return &toolingv0.ApplyEditResponse{Success: false, Failure: failures.Ensure(resp.GetFailure(), basev0.FailureCode_FAILURE_CODE_INTERNAL, "tooling.apply-edit", "code service returned no apply-edit result")}, nil
	}
	return &toolingv0.ApplyEditResponse{
		Success: ae.Success, Content: ae.Content,
		Strategy: ae.Strategy, FixActions: ae.FixActions, Failure: failures.Clone(resp.GetFailure()),
		Changed: ae.Changed, BeforeSha256: ae.BeforeSha256, AfterSha256: ae.AfterSha256,
		Wrote: ae.Wrote, Output: ae.Output,
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
		return &toolingv0.ListDependenciesResponse{Failure: failures.Ensure(resp.GetFailure(), basev0.FailureCode_FAILURE_CODE_INTERNAL, "tooling.list-dependencies", "code service returned no dependency result")}, nil
	}
	var deps []*toolingv0.Dependency
	for _, d := range ld.Dependencies {
		deps = append(deps, &toolingv0.Dependency{Name: d.Name, Version: d.Version, Direct: d.Direct})
	}
	return &toolingv0.ListDependenciesResponse{Dependencies: deps, Failure: failures.Clone(resp.GetFailure())}, nil
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
		return &toolingv0.AddDependencyResponse{Success: false, Failure: failures.Ensure(resp.GetFailure(), basev0.FailureCode_FAILURE_CODE_INTERNAL, "tooling.add-dependency", "code service returned no add-dependency result")}, nil
	}
	return &toolingv0.AddDependencyResponse{
		Success: ad.Success, InstalledVersion: ad.InstalledVersion, Failure: failures.Clone(resp.GetFailure()),
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
		return &toolingv0.RemoveDependencyResponse{Success: false, Failure: failures.Ensure(resp.GetFailure(), basev0.FailureCode_FAILURE_CODE_INTERNAL, "tooling.remove-dependency", "code service returned no remove-dependency result")}, nil
	}
	return &toolingv0.RemoveDependencyResponse{Success: rd.Success, Failure: failures.Clone(resp.GetFailure())}, nil
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
		return &toolingv0.GetProjectInfoResponse{Failure: failures.Ensure(resp.GetFailure(), basev0.FailureCode_FAILURE_CODE_INTERNAL, "tooling.get-project-info", "code service returned no project-info result")}, nil
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
		Packages: pkgs, Dependencies: deps, FileHashes: pi.FileHashes, Failure: failures.Clone(resp.GetFailure()),
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
		Failure: failures.ForOutcome(success, resp.GetStatus().GetFailure(), basev0.FailureCode_FAILURE_CODE_VALIDATION_FAILED, "tooling.test", toolingFailureSummary("tooling test", resp.GetOutput())),
	}, nil
}

func (t *Tooling) Lint(ctx context.Context, _ *toolingv0.LintRequest) (*toolingv0.LintResponse, error) {
	resp, err := t.Runtime.Lint(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("tooling lint: %w", err)
	}
	success := resp.Status != nil && resp.Status.State == runtimev0.LintStatus_SUCCESS
	return &toolingv0.LintResponse{Success: success, Output: resp.Output, Failure: failures.ForOutcome(success, resp.GetStatus().GetFailure(), basev0.FailureCode_FAILURE_CODE_VALIDATION_FAILED, "tooling.lint", toolingFailureSummary("tooling lint", resp.GetOutput()))}, nil
}

func toolingFailureSummary(operation, output string) string {
	if output == "" {
		return operation + " failed without structured status"
	}
	return output
}
