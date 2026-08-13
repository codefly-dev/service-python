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
	err := applyConfigChange(tf, &builderv0.ConfigChange{
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
