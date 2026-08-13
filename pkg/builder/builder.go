// Package builder implements the generic Python Builder gRPC service.
// Its job on the generic layer is minimal: Load/Init plumbing and Configure.
// Shared successful no-op phases come from services.DefaultBuilder.
// Specializations override Build (to produce container images) and Sync
// (to generate protos / adapters / migrations).
package builder

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/audit"
	"github.com/codefly-dev/core/agents/services/sbom"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"

	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

// Builder is the generic Python builder server. Embedded by specializations
// so they inherit Load/Init and only add what their layer needs.
type Builder struct {
	*services.DefaultBuilder
	*pythonservice.Service

	// configureMu serializes validation, mutation, and persistence as one
	// transaction. gRPC may dispatch Configure calls concurrently; without this
	// lock two valid additive repairs can overwrite one another's service model.
	configureMu sync.Mutex
}

// New builds a generic Python Builder bound to the shared Service.
func New(svc *pythonservice.Service) *Builder {
	return &Builder{
		DefaultBuilder: services.NewDefaultBuilder(svc.Builder),
		Service:        svc,
	}
}

// Load reads the identity and settings. Specializations override to wire
// endpoints and compute layer-specific paths, but should call this first.
func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()
	response, err := s.Builder.LoadService(ctx, req, services.BuilderLoad{Settings: s.Service.Settings})
	s.Service.SourceLocation = s.Service.ResolveSourceLocation()
	return response, err
}

// Init records dependency endpoints.
func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()
	s.Builder.LogInitRequest(req)
	ctx = s.Wool.Inject(ctx)
	s.DependencyEndpoints = req.DependenciesEndpoints
	return s.Builder.InitResponse()
}

// Audit is inherited by Python specializations and fails closed when the
// canonical scanner is unavailable.
func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	result, err := audit.Python(ctx, s.Service.SourceLocation, req.GetIncludeOutdated())
	if err != nil {
		return s.Builder.AuditError(err)
	}
	return s.Builder.AuditResponse(req, result.Findings, result.Outdated, result.Tool, result.Language)
}

// SBOM delegates frozen Python lock interpretation to uv's CycloneDX 1.5
// exporter. Specializations only need to set SourceLocation during Load.
func (s *Builder) SBOM(ctx context.Context, req *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	result, err := sbom.Python(ctx, s.Service.SourceLocation, req.GetIncludeDevDependencies())
	if err != nil {
		return s.Builder.SBOMError(err)
	}
	return s.Builder.SBOMResponse(result.Bom, result.Tool, result.Language, result.SHA256)
}

// Deploy fails explicitly on the generic layer. Returning a success response
// without producing a workload made the CLI report a deployment that did not
// exist. Deployable specializations override this method.
func (s *Builder) Deploy(ctx context.Context, _ *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.DeployError(fmt.Errorf("generic Python services do not define a deployable workload; use a deployable specialization such as python-fastapi"))
}

// Create likewise fails explicitly because the generic layer has no project
// template. Specializations provide their own factory tree.
func (s *Builder) Create(ctx context.Context, _ *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.CreateError(fmt.Errorf("generic Python services do not define a project template; use a concrete specialization"))
}

// Configure applies structured config changes to the service's test formula and
// PERSISTS them to service.codefly.yaml. The plugin OWNS its config file: it
// validates each change against the schema it advertises in GetAgentInformation
// (the test.provisioning knobs) and writes the file. The ordered batch is
// atomic: a rejected later change cannot leave earlier changes live in memory,
// and a failed save restores the prior formula. UNSET removes an explicit
// override so project-derived plugin defaults become effective again.
func (s *Builder) Configure(ctx context.Context, req *builderv0.ConfigureRequest) (*builderv0.ConfigureResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	s.configureMu.Lock()
	defer s.configureMu.Unlock()
	svc := s.Base.Service
	if svc == nil {
		return configureError("builder not loaded"), nil
	}
	if len(req.GetChanges()) == 0 {
		return configureError("at least one configuration change is required"), nil
	}
	candidate := cloneTestFormula(svc.Test)
	if candidate == nil {
		candidate = &resources.TestFormula{}
	}
	for _, c := range req.GetChanges() {
		if err := applyConfigChange(candidate, c); err != nil {
			return configureError(err.Error()), nil
		}
	}
	if testFormulaEmpty(candidate) {
		candidate = nil
	}
	previous := svc.Test
	svc.Test = candidate
	if err := svc.Save(ctx); err != nil {
		svc.Test = previous
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
//	test.provisioning.<key>  SET, APPEND, or UNSET
//	test.command             SET or UNSET (SET space-splits into argv)
//	test.output              SET or UNSET
//	test.env.<key>           SET or UNSET
//
// Unknown paths are rejected — the plugin only configures what it documents.
func applyConfigChange(tf *resources.TestFormula, c *builderv0.ConfigChange) error {
	if tf == nil {
		return fmt.Errorf("test formula is nil")
	}
	if c == nil {
		return fmt.Errorf("configuration change is nil")
	}
	path, val := c.GetPath(), c.GetValue()
	switch {
	case strings.HasPrefix(path, "test.provisioning."):
		key := strings.TrimPrefix(path, "test.provisioning.")
		if key == "" {
			return fmt.Errorf("empty provisioning key in path %q", path)
		}
		switch c.GetOp() {
		case builderv0.ConfigChange_SET:
			if tf.Provisioning == nil {
				tf.Provisioning = map[string]string{}
			}
			tf.Provisioning[key] = val
		case builderv0.ConfigChange_APPEND:
			if tf.Provisioning == nil {
				tf.Provisioning = map[string]string{}
			}
			tf.Provisioning[key] = appendCSV(tf.Provisioning[key], val)
		case builderv0.ConfigChange_UNSET:
			delete(tf.Provisioning, key)
			if len(tf.Provisioning) == 0 {
				tf.Provisioning = nil
			}
		default:
			return unsupportedConfigOp(c, path, "SET, APPEND, or UNSET")
		}
	case path == "test.command":
		switch c.GetOp() {
		case builderv0.ConfigChange_SET:
			tf.Command = strings.Fields(val)
		case builderv0.ConfigChange_UNSET:
			tf.Command = nil
		default:
			return unsupportedConfigOp(c, path, "SET or UNSET")
		}
	case path == "test.output":
		switch c.GetOp() {
		case builderv0.ConfigChange_SET:
			tf.Output = val
		case builderv0.ConfigChange_UNSET:
			tf.Output = ""
		default:
			return unsupportedConfigOp(c, path, "SET or UNSET")
		}
	case strings.HasPrefix(path, "test.env."):
		key := strings.TrimPrefix(path, "test.env.")
		if key == "" {
			return fmt.Errorf("empty env key in path %q", path)
		}
		switch c.GetOp() {
		case builderv0.ConfigChange_SET:
			if tf.Env == nil {
				tf.Env = map[string]string{}
			}
			tf.Env[key] = val
		case builderv0.ConfigChange_UNSET:
			delete(tf.Env, key)
			if len(tf.Env) == 0 {
				tf.Env = nil
			}
		default:
			return unsupportedConfigOp(c, path, "SET or UNSET")
		}
	default:
		return fmt.Errorf("unsupported config path %q (see GetAgentInformation configuration_details)", path)
	}
	return nil
}

func unsupportedConfigOp(c *builderv0.ConfigChange, path, allowed string) error {
	return fmt.Errorf("operation %s is not valid for %q; use %s", c.GetOp(), path, allowed)
}

func cloneTestFormula(tf *resources.TestFormula) *resources.TestFormula {
	if tf == nil {
		return nil
	}
	clone := &resources.TestFormula{
		Command: append([]string(nil), tf.Command...),
		Output:  tf.Output,
	}
	if len(tf.Env) > 0 {
		clone.Env = make(map[string]string, len(tf.Env))
		for key, value := range tf.Env {
			clone.Env[key] = value
		}
	}
	if len(tf.Provisioning) > 0 {
		clone.Provisioning = make(map[string]string, len(tf.Provisioning))
		for key, value := range tf.Provisioning {
			clone.Provisioning[key] = value
		}
	}
	return clone
}

func testFormulaEmpty(tf *resources.TestFormula) bool {
	return tf == nil || (len(tf.Command) == 0 && tf.Output == "" && len(tf.Env) == 0 && len(tf.Provisioning) == 0)
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
		for _, k := range sortedKeys(tf.Provisioning) {
			v := tf.Provisioning[k]
			fmt.Fprintf(&b, "    %s: %s\n", k, v)
		}
	}
	if len(tf.Env) > 0 {
		b.WriteString("  env:\n")
		for _, k := range sortedKeys(tf.Env) {
			fmt.Fprintf(&b, "    %s: %s\n", k, tf.Env[k])
		}
	}
	return b.String()
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
