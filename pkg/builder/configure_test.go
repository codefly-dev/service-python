package builder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"

	pythonservice "github.com/codefly-dev/service-python/pkg/service"
)

func TestConfigureBatchIsAtomicAndUnsetRestoresDerivation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	configured := &resources.Service{
		Name:    "python-unit",
		Version: "0.0.0",
		Agent:   &resources.Agent{Kind: "codefly:service", Name: "python"},
		Test: &resources.TestFormula{
			Provisioning: map[string]string{"editable": "false", "with": "pytest"},
			Env:          map[string]string{"CFLAGS": "-Werror"},
		},
	}
	configured.WithDir(dir)
	if err := configured.Save(ctx); err != nil {
		t.Fatal(err)
	}

	svc := pythonservice.New(configured.Agent)
	svc.Base.Service = configured
	b := New(svc)

	reset, err := b.Configure(ctx, &builderv0.ConfigureRequest{Changes: []*builderv0.ConfigChange{
		{Path: "test.provisioning.editable", Op: builderv0.ConfigChange_UNSET},
		{Path: "test.provisioning.with", Value: "hypothesis", Op: builderv0.ConfigChange_APPEND},
		{Path: "test.env.CFLAGS", Op: builderv0.ConfigChange_UNSET},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if reset.GetState().GetState() != builderv0.ConfigureStatus_SUCCESS {
		t.Fatalf("reset configuration failed: %+v", reset.GetState())
	}
	if _, exists := configured.Test.Provisioning["editable"]; exists {
		t.Fatalf("editable override survived UNSET: %v", configured.Test.Provisioning)
	}
	if got := configured.Test.Provisioning["with"]; got != "pytest,hypothesis" {
		t.Fatalf("with = %q, want ordered idempotent append", got)
	}
	if len(configured.Test.Env) != 0 {
		t.Fatalf("environment override survived UNSET: %v", configured.Test.Env)
	}

	before, err := os.ReadFile(filepath.Join(dir, resources.ServiceConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := b.Configure(ctx, &builderv0.ConfigureRequest{Changes: []*builderv0.ConfigChange{
		{Path: "test.provisioning.python", Value: "3.11", Op: builderv0.ConfigChange_SET},
		{Path: "test.env.CFLAGS", Value: "-O0", Op: builderv0.ConfigChange_APPEND},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.GetState().GetState() != builderv0.ConfigureStatus_ERROR {
		t.Fatalf("invalid batch was accepted: %+v", rejected.GetState())
	}
	if _, exists := configured.Test.Provisioning["python"]; exists {
		t.Fatalf("rejected batch partially mutated live configuration: %v", configured.Test.Provisioning)
	}
	after, err := os.ReadFile(filepath.Join(dir, resources.ServiceConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("rejected batch changed persisted configuration\nbefore:\n%s\nafter:\n%s", before, after)
	}

	cleared, err := b.Configure(ctx, &builderv0.ConfigureRequest{Changes: []*builderv0.ConfigChange{
		{Path: "test.provisioning.with", Op: builderv0.ConfigChange_UNSET},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.GetState().GetState() != builderv0.ConfigureStatus_SUCCESS {
		t.Fatalf("clear configuration failed: %+v", cleared.GetState())
	}
	if configured.Test != nil {
		t.Fatalf("empty explicit formula must be removed so project derivation owns the baseline: %+v", configured.Test)
	}
	persisted, err := os.ReadFile(filepath.Join(dir, resources.ServiceConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "test:") {
		t.Fatalf("empty test override persisted instead of exposing derivation:\n%s", persisted)
	}
}

func TestApplyConfigChangeRejectsUnknownOperation(t *testing.T) {
	tf := &resources.TestFormula{}
	err := applyConfigChange(t.TempDir(), tf, &builderv0.ConfigChange{
		Path:  "test.provisioning.python",
		Value: "3.11",
		Op:    builderv0.ConfigChange_UNKNOWN,
	})
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("unknown operation error = %v", err)
	}
	if !testFormulaEmpty(tf) {
		t.Fatalf("rejected operation mutated formula: %+v", tf)
	}
}

func TestApplyConfigChangeEnforcesAdvertisedProvisioningSemantics(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "requirements", "test.txt"), []byte("pytest\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	valid := []struct {
		path  string
		value string
		op    builderv0.ConfigChange_Op
	}{
		{path: "test.provisioning.with", value: "pyerfa>=2.0.1.1", op: builderv0.ConfigChange_APPEND},
		{path: "test.provisioning.requirements", value: "requirements/test.txt", op: builderv0.ConfigChange_APPEND},
		{path: "test.provisioning.no_build_isolation", value: "true", op: builderv0.ConfigChange_SET},
		{path: "test.provisioning.exclude_newer", value: "2025-01-01T00:00:00Z", op: builderv0.ConfigChange_SET},
	}
	for _, test := range valid {
		t.Run("valid_"+strings.ReplaceAll(test.path, ".", "_"), func(t *testing.T) {
			formula := &resources.TestFormula{}
			if err := applyConfigChange(root, formula, &builderv0.ConfigChange{Path: test.path, Value: test.value, Op: test.op}); err != nil {
				t.Fatalf("valid change rejected: %v", err)
			}
		})
	}

	invalid := []struct {
		name  string
		path  string
		value string
		op    builderv0.ConfigChange_Op
	}{
		{name: "unknown key", path: "test.provisioning.typo", value: "true", op: builderv0.ConfigChange_SET},
		{name: "package in requirement files", path: "test.provisioning.requirements", value: "pyerfa>=2.0.1.1", op: builderv0.ConfigChange_APPEND},
		{name: "empty requirement file list", path: "test.provisioning.requirements", value: ",", op: builderv0.ConfigChange_APPEND},
		{name: "placeholder package", path: "test.provisioning.with", value: "<code-unit-root>", op: builderv0.ConfigChange_APPEND},
		{name: "local path package", path: "test.provisioning.with", value: ".", op: builderv0.ConfigChange_APPEND},
		{name: "invalid boolean", path: "test.provisioning.editable", value: "yes", op: builderv0.ConfigChange_SET},
		{name: "append scalar", path: "test.provisioning.python", value: "3.10", op: builderv0.ConfigChange_APPEND},
		{name: "invalid timestamp", path: "test.provisioning.exclude_newer", value: "tomorrow", op: builderv0.ConfigChange_SET},
		{name: "missing cwd", path: "test.provisioning.cwd", value: "missing", op: builderv0.ConfigChange_SET},
		{name: "empty extra list", path: "test.provisioning.extras", value: ",", op: builderv0.ConfigChange_APPEND},
		{name: "valued unset", path: "test.provisioning.with", value: "pytest", op: builderv0.ConfigChange_UNSET},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			formula := &resources.TestFormula{}
			err := applyConfigChange(root, formula, &builderv0.ConfigChange{Path: test.path, Value: test.value, Op: test.op})
			if err == nil {
				t.Fatalf("invalid change was accepted: %+v", test)
			}
			if !testFormulaEmpty(formula) {
				t.Fatalf("rejected change mutated formula: %+v", formula)
			}
		})
	}
}

func TestConfigureRejectsCrossTypedValueBeforePersistence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	configured := &resources.Service{
		Name:    "python-unit",
		Version: "0.0.0",
		Agent:   &resources.Agent{Kind: "codefly:service", Name: "python"},
	}
	configured.WithDir(root)
	if err := configured.Save(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, resources.ServiceConfigurationName))
	if err != nil {
		t.Fatal(err)
	}

	svc := pythonservice.New(configured.Agent)
	svc.Base.Service = configured
	svc.SourceLocation = root
	response, err := New(svc).Configure(ctx, &builderv0.ConfigureRequest{Changes: []*builderv0.ConfigChange{{
		Path: "test.provisioning.requirements", Value: "pyerfa>=2.0.1.1", Op: builderv0.ConfigChange_APPEND,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetState().GetState() != builderv0.ConfigureStatus_ERROR {
		t.Fatalf("cross-typed value was accepted: %+v", response)
	}
	if message := response.GetState().GetMessage(); !strings.Contains(message, "requirement files") || !strings.Contains(message, "pyerfa>=2.0.1.1") {
		t.Fatalf("rejection is not actionable: %q", message)
	}
	if configured.Test != nil {
		t.Fatalf("rejected configuration mutated live service: %+v", configured.Test)
	}
	after, err := os.ReadFile(filepath.Join(root, resources.ServiceConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("rejected configuration changed persistence\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestConfigureSerializesConcurrentAdditiveRepairs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	configured := &resources.Service{
		Name:    "python-unit",
		Version: "0.0.0",
		Agent:   &resources.Agent{Kind: "codefly:service", Name: "python"},
	}
	configured.WithDir(dir)
	if err := configured.Save(ctx); err != nil {
		t.Fatal(err)
	}
	svc := pythonservice.New(configured.Agent)
	svc.Base.Service = configured
	b := New(svc)

	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, dependency := range []string{"pytest", "hypothesis"} {
		dependency := dependency
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			response, err := b.Configure(ctx, &builderv0.ConfigureRequest{Changes: []*builderv0.ConfigChange{{
				Path: "test.provisioning.with", Value: dependency, Op: builderv0.ConfigChange_APPEND,
			}}})
			if err != nil {
				errors <- err
				return
			}
			if response.GetState().GetState() != builderv0.ConfigureStatus_SUCCESS {
				errors <- fmt.Errorf("configure %s: %s", dependency, response.GetState().GetMessage())
			}
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}

	dependencies := strings.Split(configured.Test.Provisioning["with"], ",")
	sort.Strings(dependencies)
	if got := strings.Join(dependencies, ","); got != "hypothesis,pytest" {
		t.Fatalf("concurrent additive repairs = %q, want both values", got)
	}
	reloaded, err := resources.LoadServiceFromDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	persisted := strings.Split(reloaded.Test.Provisioning["with"], ",")
	sort.Strings(persisted)
	if got := strings.Join(persisted, ","); got != "hypothesis,pytest" {
		t.Fatalf("persisted concurrent repairs = %q, want both values", got)
	}
}
