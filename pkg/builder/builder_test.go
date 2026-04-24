package builder_test

import (
	"testing"

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
