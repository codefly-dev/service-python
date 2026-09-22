package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestConformanceUsesExistingPythonSource(t *testing.T) {
	data, err := os.ReadFile("agent.codefly.yaml")
	require.NoError(t, err)
	var manifest struct {
		Conformance struct {
			Mode    string `yaml:"mode"`
			Fixture string `yaml:"fixture"`
		} `yaml:"conformance"`
	}
	require.NoError(t, yaml.Unmarshal(data, &manifest))
	require.Equal(t, "attach-existing-source", manifest.Conformance.Mode)
	require.True(t, filepath.IsLocal(manifest.Conformance.Fixture))
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), manifest.Conformance.Fixture)
	require.NoError(t, err)
	module, err := workspace.RootModule(t.Context())
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "subject")
	require.NoError(t, err)
	require.Equal(t, "codefly.dev", service.Agent.Publisher)
	require.Equal(t, "python", service.Agent.Name)
	require.Equal(t, "latest", service.Agent.Version, "the isolated release gate must exercise its candidate")
	command := exec.CommandContext(t.Context(), "uv", "run", "--frozen", "python", "-m", "unittest", "discover")
	command.Dir = filepath.Join(manifest.Conformance.Fixture, "services", "subject", "code")
	command.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(t.TempDir(), "venv"), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	require.Contains(t, string(output), "Ran 1 test")
}
