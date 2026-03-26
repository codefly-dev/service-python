package main

import (
	"context"
	"os"
	"os/exec"

	"github.com/codefly-dev/core/agents/services"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	pythonhelpers "github.com/codefly-dev/core/runners/python"
)

type Runtime struct {
	services.RuntimeServer
	*Service
}

func NewRuntime(svc *Service) *Runtime {
	return &Runtime{
		Service: svc,
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading base")
	}

	s.Runtime.SetEnvironment(req.Environment)

	// Source is in the service directory itself (Python convention)
	s.sourceLocation = s.Location
	if wd := os.Getenv("CODEFLY_AGENT_WORKDIR"); wd != "" {
		s.sourceLocation = wd
	}

	return s.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()

	// Ensure venv exists via uv
	cmd := exec.CommandContext(ctx, "uv", "sync")
	cmd.Dir = s.sourceLocation
	cmd.Stdout = s.Logger
	cmd.Stderr = s.Logger
	if err := cmd.Run(); err != nil {
		s.Wool.Warn("uv sync failed (non-fatal)", nil)
	}

	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	return s.Runtime.StartResponse()
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	defer s.Wool.Catch()

	s.Infof("running python tests")

	summary, runErr := pythonhelpers.RunPythonTests(ctx, s.sourceLocation, nil)

	s.Wool.Forwardf("Tests: %s", summary.SummaryLine())
	for _, f := range summary.Failures {
		s.Wool.Forwardf("%s", f)
	}

	return s.Runtime.TestResponseWithResults(summary.Run, summary.Passed, summary.Failed, summary.Skipped, summary.Coverage, summary.Failures, runErr)
}

func (s *Runtime) Lint(ctx context.Context, req *runtimev0.LintRequest) (*runtimev0.LintResponse, error) {
	defer s.Wool.Catch()

	output, err := pythonhelpers.RunPythonLint(ctx, s.sourceLocation)

	return &runtimev0.LintResponse{
		Status: &runtimev0.LintStatus{
			State:   boolToLintState(err == nil),
			Message: output,
		},
	}, nil
}

func (s *Runtime) Build(ctx context.Context, req *runtimev0.BuildRequest) (*runtimev0.BuildResponse, error) {
	// Python doesn't need a build step — uv sync handles deps
	return &runtimev0.BuildResponse{}, nil
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func boolToLintState(ok bool) runtimev0.LintStatus_Status {
	if ok {
		return runtimev0.LintStatus_SUCCESS
	}
	return runtimev0.LintStatus_ERROR
}
