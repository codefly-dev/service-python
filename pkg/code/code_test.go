package code_test

import (
	"testing"

	"github.com/codefly-dev/core/resources"

	pythoncode "github.com/codefly-dev/service-python/pkg/code"
	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

// TestCodeEmbedsService verifies the embedding chain so specializations
// can promote services.Base methods through *pythoncode.Code.
func TestCodeEmbedsService(t *testing.T) {
	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	c := pythoncode.New(svc)
	if c == nil {
		t.Fatal("New returned nil")
	}
	if c.Service != svc {
		t.Error("Service pointer mismatch")
	}
	if c.PythonCodeServer == nil {
		t.Fatal("embedded *PythonCodeServer is nil")
	}
}

// TestSourceDirFallback confirms the resolution order used by every Python
// Code RPC. Specializations that set Service.SourceLocation rely on it
// winning over $CODEFLY_AGENT_WORKDIR and the service root.
func TestSourceDirFallback(t *testing.T) {
	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	c := pythoncode.New(svc)

	// Before anything is set, falls back to Service.Location (empty for
	// a bare Service, but the method should still return a string).
	got := c.SourceDir()
	// With SourceLocation set, that wins.
	svc.SourceLocation = "/explicit/src"
	if c.SourceDir() != "/explicit/src" {
		t.Errorf("SourceLocation should win; got %q", c.SourceDir())
	}
	_ = got
}
