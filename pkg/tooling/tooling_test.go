package tooling_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	toolingv0 "github.com/codefly-dev/core/generated/go/codefly/services/tooling/v0"
	"github.com/codefly-dev/core/resources"

	pythoncode "github.com/codefly-dev/service-python/pkg/code"
	pythonruntime "github.com/codefly-dev/service-python/pkg/runtime"
	pythonservice "github.com/codefly-dev/service-python/pkg/service"
	pythontooling "github.com/codefly-dev/service-python/pkg/tooling"
)

// TestToolingWiring verifies Tooling holds the Code and Runtime pointers
// the caller supplied.
func TestToolingWiring(t *testing.T) {
	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	c := pythoncode.New(svc)
	rt := pythonruntime.New(svc)

	tl := pythontooling.New(c, rt)
	if tl == nil {
		t.Fatal("New returned nil")
	}
	if tl.Code != c {
		t.Error("Tooling.Code is not the Code passed to New")
	}
	if tl.Runtime != rt {
		t.Error("Tooling.Runtime is not the Runtime passed to New")
	}
}

func TestProjectInfoCarriesRequirementsAndImportsAcrossCodeAndTooling(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.in"), []byte("flask==3.1.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("import flask\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	service.SourceLocation = dir
	server := pythoncode.New(service)

	codeResponse, err := server.Execute(context.Background(), &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GetProjectInfo{GetProjectInfo: &codev0.GetProjectInfoRequest{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	project := codeResponse.GetGetProjectInfo()
	if codeResponse.GetFailure() != nil || len(project.GetDependencies()) != 1 || project.GetDependencies()[0].GetName() != "flask" ||
		project.GetDependencies()[0].GetVersion() != "==3.1.2" || len(project.GetSourceFiles()) != 1 ||
		project.GetSourceFiles()[0].GetPath() != "app.py" || len(project.GetSourceFiles()[0].GetImports()) != 1 || project.GetSourceFiles()[0].GetImports()[0] != "flask" {
		t.Fatalf("code project info = %+v failure=%+v", project, codeResponse.GetFailure())
	}

	toolingResponse, err := pythontooling.New(server, nil).GetProjectInfo(context.Background(), &toolingv0.GetProjectInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if toolingResponse.GetFailure() != nil || len(toolingResponse.GetDependencies()) != 1 || toolingResponse.GetDependencies()[0].GetName() != "flask" ||
		len(toolingResponse.GetSourceFiles()) != 1 || toolingResponse.GetSourceFiles()[0].GetPath() != "app.py" ||
		len(toolingResponse.GetSourceFiles()[0].GetImports()) != 1 || toolingResponse.GetSourceFiles()[0].GetImports()[0] != "flask" {
		t.Fatalf("tooling project info = %+v", toolingResponse)
	}
	semantic, err := pythontooling.New(server, nil).GetSemanticIndex(context.Background(), &toolingv0.GetSemanticIndexRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if semantic.GetFailure() != nil || semantic.GetIndex().GetState() != basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_COMPLETE || len(semantic.GetIndex().GetFiles()) != 1 {
		t.Fatalf("semantic index = %+v", semantic)
	}
}
