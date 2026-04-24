package tooling_test

import (
	"testing"

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
