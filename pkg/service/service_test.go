package service_test

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/codefly-dev/core/resources"

	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

// TestNew verifies a generic Python Service can be constructed and carries
// a non-nil Base + Settings. This is the foundation every specialization
// embeds; if this breaks, every downstream agent breaks.
func TestNew(t *testing.T) {
	svc := pythonservice.New(&resources.Agent{
		Kind:      "codefly:service",
		Publisher: "codefly.dev",
		Name:      "python",
		Version:   "0.0.1",
	})
	if svc == nil {
		t.Fatal("New returned nil")
	}
	if svc.Base == nil {
		t.Fatal("Service.Base is nil — services.Base embedding broken")
	}
	if svc.Settings == nil {
		t.Fatal("Service.Settings is nil")
	}
	if svc.SourceLocation != "" {
		t.Fatalf("SourceLocation should be empty before Load, got %q", svc.SourceLocation)
	}
}

// TestSettingsYAMLInline proves the inline-embed pattern specializations
// rely on: a specialization's Settings struct can embed pythonservice.Settings
// with yaml:",inline" and unmarshalling produces a flat YAML shape.
//
// Specializations depend on this: if we ever change Settings to non-inline,
// every specialization's YAML fixtures silently break.
func TestSettingsYAMLInline(t *testing.T) {
	type fastapiSettings struct {
		pythonservice.Settings `yaml:",inline"`
		HotReload              bool `yaml:"hot-reload"`
	}

	src := []byte(`
python-version: "3.12"
hot-reload: true
`)

	var s fastapiSettings
	if err := yaml.Unmarshal(src, &s); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if s.PythonVersion != "3.12" {
		t.Errorf("inherited PythonVersion not populated: got %q want 3.12", s.PythonVersion)
	}
	if !s.HotReload {
		t.Error("local HotReload not populated")
	}
}

// TestGetAgentInformationGeneric asserts the generic advertisement shape.
// Specializations override this, so the test locks in the *generic*
// contract — break this and callers assuming a generic python plugin is
// capability-BUILDER+RUNTIME and language-PYTHON will notice.
func TestGetAgentInformationGeneric(t *testing.T) {
	svc := pythonservice.New(&resources.Agent{Kind: "codefly:service", Name: "python"})
	info, err := svc.GetAgentInformation(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetAgentInformation: %v", err)
	}
	if len(info.Languages) != 1 || info.Languages[0].Type.String() != "PYTHON" {
		t.Errorf("expected single PYTHON language, got %+v", info.Languages)
	}
	if len(info.Protocols) != 0 {
		t.Errorf("generic python should advertise no protocols, got %d", len(info.Protocols))
	}
}
