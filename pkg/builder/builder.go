// Package builder implements the generic Python Builder gRPC service.
// Its job on the generic layer is minimal: Load/Init plumbing, a no-op
// Build (uv sync happens at Runtime.Init), a no-op Sync (no protos).
// Specializations override Build (to produce container images) and Sync
// (to generate protos / adapters / migrations).
package builder

import (
	"context"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"

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
