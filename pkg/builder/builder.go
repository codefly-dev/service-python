// Package builder implements the generic Python Builder gRPC service.
// Its job on the generic layer is minimal: Load/Init plumbing, a no-op
// Build (uv sync happens at Runtime.Init), a no-op Sync (no protos).
// Specializations override Build (to produce container images) and Sync
// (to generate protos / adapters / migrations).
package builder

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"

	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

// Builder is the generic Python builder server. Embedded by specializations
// so they inherit Load/Init and only add what their layer needs.
type Builder struct {
	services.BuilderServer
	*pythonservice.Service
}

// New builds a generic Python Builder bound to the shared Service.
func New(svc *pythonservice.Service) *Builder {
	return &Builder{Service: svc}
}

// Load reads the identity and settings. Specializations override to wire
// endpoints and compute layer-specific paths, but should call this first.
func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if err := s.Builder.Load(ctx, req.Identity, s.Service.Settings); err != nil {
		return s.Builder.LoadError(err)
	}

	s.Service.SourceLocation = s.Location

	if req.CreationMode != nil {
		s.Builder.CreationMode = req.CreationMode
		return s.Builder.LoadResponse()
	}

	s.Endpoints, _ = s.Base.Service.LoadEndpoints(ctx)
	return s.Builder.LoadResponse()
}

// Init records dependency endpoints.
func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()
	s.Builder.LogInitRequest(req)
	ctx = s.Wool.Inject(ctx)
	s.DependencyEndpoints = req.DependenciesEndpoints
	return s.Builder.InitResponse()
}

// Update is a no-op on the generic layer.
func (s *Builder) Update(ctx context.Context, _ *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return &builderv0.UpdateResponse{}, nil
}

// Sync is a no-op on the generic layer — Python has no protos to generate.
// Specializations (python-fastapi, python-grpc) override to regenerate
// their protocol artifacts.
func (s *Builder) Sync(ctx context.Context, _ *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.SyncResponse()
}

// Build is a no-op on the generic layer — uv sync at Runtime.Init handles
// Python dep resolution; there's no artifact to produce. Specializations
// override to build container images.
func (s *Builder) Build(ctx context.Context, _ *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.BuildResponse()
}

// Deploy is a no-op on the generic layer. Specializations override to
// render k8s manifests or push images.
func (s *Builder) Deploy(ctx context.Context, _ *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.DeployResponse()
}

// Create is a no-op on the generic layer. A generic "python service" has
// no inherent template — it's an abstract starting point. Specializations
// ship their own templates/factory/ tree and override Create to apply it.
func (s *Builder) Create(ctx context.Context, _ *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.CreateResponse(ctx, s.Service.Settings)
}

// Configure applies structured config changes to the service's test formula and
// PERSISTS them to service.codefly.yaml. The plugin OWNS its config file: it
// validates each change against the schema it advertises in GetAgentInformation
// (the test.provisioning knobs) and writes the file. This is the write-action the
// LLM / tooling inner loop calls to persist a learned fix (e.g. add a dependency
// pin after an env-blocked test run).
func (s *Builder) Configure(ctx context.Context, req *builderv0.ConfigureRequest) (*builderv0.ConfigureResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	svc := s.Base.Service
	if svc == nil {
		return configureError("builder not loaded"), nil
	}
	if svc.Test == nil {
		svc.Test = &resources.TestFormula{}
	}
	for _, c := range req.GetChanges() {
		if err := applyConfigChange(svc.Test, c); err != nil {
			return configureError(err.Error()), nil
		}
	}
	if err := svc.Save(ctx); err != nil {
		return configureError("persist service.codefly.yaml: " + err.Error()), nil
	}
	return &builderv0.ConfigureResponse{
		State: &builderv0.ConfigureStatus{
			State:   builderv0.ConfigureStatus_SUCCESS,
			Message: fmt.Sprintf("applied %d change(s)", len(req.GetChanges())),
		},
		EffectiveYaml: renderTestFormula(svc.Test),
	}, nil
}

func configureError(msg string) *builderv0.ConfigureResponse {
	return &builderv0.ConfigureResponse{
		State: &builderv0.ConfigureStatus{State: builderv0.ConfigureStatus_ERROR, Message: msg},
	}
}

// applyConfigChange mutates the service's TestFormula at a dotted path. Supported
// paths mirror the schema GetAgentInformation advertises:
//
//	test.provisioning.<key>  SET or APPEND (APPEND folds into the comma list)
//	test.command             SET (space-split into argv)
//	test.output              SET
//	test.env.<key>           SET
//
// Unknown paths are rejected — the plugin only configures what it documents.
func applyConfigChange(tf *resources.TestFormula, c *builderv0.ConfigChange) error {
	path, val := c.GetPath(), c.GetValue()
	switch {
	case strings.HasPrefix(path, "test.provisioning."):
		key := strings.TrimPrefix(path, "test.provisioning.")
		if key == "" {
			return fmt.Errorf("empty provisioning key in path %q", path)
		}
		if tf.Provisioning == nil {
			tf.Provisioning = map[string]string{}
		}
		if c.GetOp() == builderv0.ConfigChange_APPEND {
			tf.Provisioning[key] = appendCSV(tf.Provisioning[key], val)
		} else {
			tf.Provisioning[key] = val
		}
	case path == "test.command":
		tf.Command = strings.Fields(val)
	case path == "test.output":
		tf.Output = val
	case strings.HasPrefix(path, "test.env."):
		key := strings.TrimPrefix(path, "test.env.")
		if key == "" {
			return fmt.Errorf("empty env key in path %q", path)
		}
		if tf.Env == nil {
			tf.Env = map[string]string{}
		}
		tf.Env[key] = val
	default:
		return fmt.Errorf("unsupported config path %q (see GetAgentInformation configuration_details)", path)
	}
	return nil
}

// appendCSV adds val to a comma-separated list, skipping it if already present.
func appendCSV(cur, val string) string {
	val = strings.TrimSpace(val)
	if val == "" {
		return cur
	}
	for _, e := range strings.Split(cur, ",") {
		if strings.TrimSpace(e) == val {
			return cur
		}
	}
	if strings.TrimSpace(cur) == "" {
		return val
	}
	return cur + "," + val
}

// renderTestFormula renders the persisted test formula as a compact, human/LLM
// readable summary for ConfigureResponse.effective_yaml (no yaml dep needed).
func renderTestFormula(tf *resources.TestFormula) string {
	if tf == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "test:\n  command: %s\n", strings.Join(tf.Command, " "))
	if tf.Output != "" {
		fmt.Fprintf(&b, "  output: %s\n", tf.Output)
	}
	if len(tf.Provisioning) > 0 {
		b.WriteString("  provisioning:\n")
		for k, v := range tf.Provisioning {
			fmt.Fprintf(&b, "    %s: %s\n", k, v)
		}
	}
	return b.String()
}
