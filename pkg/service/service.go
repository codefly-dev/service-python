// Package service defines the generic Python agent's shared state.
// Specializations (python-fastapi, python-grpc, …) embed *Service
// in their own Service and add protocol-specific fields.
package service

import (
	"context"

	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
)

// Settings is the generic Python agent's configuration block. Specializations
// embed this inline to inherit the fields:
//
//	type Settings struct {
//	    pythonservice.Settings `yaml:",inline"`
//	    // fastapi-specific fields here
//	}
type Settings struct {
	PythonVersion string `yaml:"python-version"`
}

// Service carries the shared state used by Runtime, Code, Tooling, Builder.
// Exported fields are the inheritance surface — specializations read and
// write SourceLocation, compose Settings, etc.
type Service struct {
	*services.Base
	Settings *Settings

	// SourceLocation is the on-disk path to the service source (set during
	// Load). Specializations may override at Load time.
	SourceLocation string

	// ActiveEnv is the plugin's active RunnerEnvironment — set by
	// Runtime.Init via CreateRunnerEnvironment and consumed by Code /
	// Tooling so every spawn routes through the same mode (native /
	// docker / nix). Nil before Runtime.Init — call sites fall back to
	// a fresh NativeEnvironment for pre-init ops.
	ActiveEnv runners.RunnerEnvironment
}

// New builds a generic Python Service bound to the given agent manifest.
func New(agent *resources.Agent) *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent),
		Settings: &Settings{},
	}
}

// GetAgentInformation returns the generic Python capability advertisement.
// Specializations SHOULD override this to add protocols/techniques relevant
// to their layer — call this method from the overriding method if they want
// to inherit the base languages/runtime requirements.
func (s *Service) GetAgentInformation(_ context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	return &agentv0.AgentInformation{
		RuntimeRequirements: []*agentv0.Runtime{
			{Type: agentv0.Runtime_NIX},
		},
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_BUILDER},
			{Type: agentv0.Capability_RUNTIME},
		},
		Languages: []*agentv0.Language{
			{Type: agentv0.Language_PYTHON},
		},
		Protocols: []*agentv0.Protocol{},
		ReadMe:    "Generic Python service managed by uv. Supports Nix runtime.",
		// The KNOWLEDGE DUMP: what this plugin can be configured with, so the LLM
		// (and the tooling inner loop) can produce the RIGHT fix and persist it via
		// Configure into service.codefly.yaml. These are the test.provisioning knobs
		// the python runner maps to uv flags (SpecFromFormula).
		ConfigurationDetails: []*agentv0.ConfigurationValueDetail{{
			Name:        "test.provisioning",
			Description: "uv provisioning for the test run. Set these to make a project's tests installable/runnable; the runner maps them to `uv run` flags. Multi-valued fields (with, requirements) are comma-joined.",
			Fields: []*agentv0.ConfigurationValueInformation{
				{Name: "python", Description: "CPython version, e.g. \"3.9\" → uv --python."},
				{Name: "editable", Description: "\"true\" installs the project itself editable → uv --with-editable ."},
				{Name: "requirements", Description: "Comma-separated requirement files → uv --with-requirements (e.g. \"requirements/tests.txt\")."},
				{Name: "with", Description: "Comma-separated extra package specs / pins → uv --with (e.g. \"pytest,Werkzeug<3\"). This is where a missing dep or version constraint is added."},
				{Name: "no_project", Description: "\"true\" runs without the project's own lockfile resolution → uv --no-project."},
			},
		}},
		// LLM-facing remediation technique: how to heal an env-blocked test run by
		// editing config — the exact loop the tooling inner loop runs.
		Techniques: []*agentv0.AgentTechnique{{
			Id:          "heal-test-environment",
			Name:        "Heal a blocked test environment",
			Description: "When a test run returns Result.State=ERRORED (env-blocked: missing-dependency / version-conflict / import-error), the tests could not run — it is NOT a test failure. Fix the environment, do not edit the patch.",
			Tags:        []string{"testing", "provisioning", "remediation"},
			Prompt:      "The test run was blocked by the environment, not by failing tests. Read the error detail (it names the missing package or version conflict). Add the fix to the service's test.provisioning via Configure: for a missing dependency or version conflict, APPEND the spec to `test.provisioning.with` (e.g. \"Werkzeug<3\"); for missing test deps, add the file to `test.provisioning.requirements`; for a wrong interpreter, set `test.provisioning.python`. Then re-run the tests. Persist only fixes that make the tests execute.",
		}},
	}, nil
}
