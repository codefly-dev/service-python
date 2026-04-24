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
	}, nil
}
