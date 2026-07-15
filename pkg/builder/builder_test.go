package builder_test

import (
	"context"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"

	pythonbuilder "github.com/codefly-dev/service-python/pkg/builder"
	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

// TestBuilderEmbedsService verifies the embedding chain so specializations
// can promote services.Base methods through *pythonbuilder.Builder when
// they embed it.
func TestBuilderEmbedsService(t *testing.T) {
	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	b := pythonbuilder.New(svc)
	if b == nil {
		t.Fatal("New returned nil")
	}
	if b.Service != svc {
		t.Error("embedded Service is not the same pointer passed to New")
	}
	_ = b.Base
	_ = b.Builder
}

func TestGenericBuilderRejectsUnsupportedCreateAndDeploy(t *testing.T) {
	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	b := pythonbuilder.New(svc)

	created, err := b.Create(context.Background(), &builderv0.CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetState().GetState() != builderv0.CreateStatus_ERROR || !strings.Contains(created.GetState().GetMessage(), "concrete specialization") {
		t.Fatalf("unexpected create response: %+v", created)
	}

	deployed, err := b.Deploy(context.Background(), &builderv0.DeploymentRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if deployed.GetState().GetState() != builderv0.DeploymentStatus_ERROR || !strings.Contains(deployed.GetState().GetMessage(), "python-fastapi") {
		t.Fatalf("unexpected deploy response: %+v", deployed)
	}
}
