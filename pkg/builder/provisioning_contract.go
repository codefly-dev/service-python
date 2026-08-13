package builder

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	piprequirements "github.com/scagogogo/python-requirements-parser/pkg/parser"
)

// ARCHITECTURE: The Python plugin owns the meaning of its provisioning bag.
// Validate that contract here, at Configure's persistence boundary, so Mind and
// other callers stay toolchain-blind and receive an actionable typed rejection
// before an invalid experiment reaches the expensive runtime probe.
func validateProvisioningChange(sourceRoot, key string, change *builderv0.ConfigChange) error {
	path := "test.provisioning." + key
	op := change.GetOp()
	if op == builderv0.ConfigChange_UNSET {
		if !supportedProvisioningKey(key) {
			return fmt.Errorf("unsupported config path %q (see GetAgentInformation configuration_details)", path)
		}
		if strings.TrimSpace(change.GetValue()) != "" {
			return fmt.Errorf("UNSET %q must omit value", path)
		}
		return nil
	}

	value := strings.TrimSpace(change.GetValue())
	if value == "" {
		return fmt.Errorf("%s %q requires a non-empty value", op, path)
	}
	switch key {
	case "python":
		if op != builderv0.ConfigChange_SET {
			return unsupportedConfigOp(change, path, "SET or UNSET")
		}
		if strings.ContainsAny(value, "<>/\\") {
			return fmt.Errorf("%q must be a CPython version, got %q", path, value)
		}
	case "exclude_newer":
		if op != builderv0.ConfigChange_SET {
			return unsupportedConfigOp(change, path, "SET or UNSET")
		}
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("%q must be an RFC3339 timestamp: %w", path, err)
		}
	case "editable", "no_project", "no_build_isolation", "persistent_venv":
		if op != builderv0.ConfigChange_SET {
			return unsupportedConfigOp(change, path, "SET or UNSET")
		}
		if value != "true" && value != "false" {
			return fmt.Errorf("%q must be \"true\" or \"false\", got %q", path, value)
		}
	case "requirements":
		if op != builderv0.ConfigChange_SET && op != builderv0.ConfigChange_APPEND {
			return unsupportedConfigOp(change, path, "SET, APPEND, or UNSET")
		}
		values := splitConfigList(value)
		if len(values) == 0 {
			return fmt.Errorf("%q requires at least one requirement file path", path)
		}
		for _, relative := range values {
			if err := validateCodeUnitPath(sourceRoot, relative, false); err != nil {
				return fmt.Errorf("%q accepts code-unit-relative requirement files; %q is invalid: %w", path, relative, err)
			}
		}
	case "with":
		if op != builderv0.ConfigChange_SET && op != builderv0.ConfigChange_APPEND {
			return unsupportedConfigOp(change, path, "SET, APPEND, or UNSET")
		}
		if err := validatePackageRequirement(value); err != nil {
			return fmt.Errorf("%q accepts one Python package requirement spec, got %q: %w", path, value, err)
		}
	case "dependency_groups", "extras":
		if op != builderv0.ConfigChange_SET && op != builderv0.ConfigChange_APPEND {
			return unsupportedConfigOp(change, path, "SET, APPEND, or UNSET")
		}
		values := splitConfigList(value)
		if len(values) == 0 {
			return fmt.Errorf("%q requires at least one project-declared name", path)
		}
		for _, name := range values {
			if strings.ContainsAny(name, "<>/\\") {
				return fmt.Errorf("%q accepts project-declared names, got %q", path, name)
			}
		}
	case "cwd":
		if op != builderv0.ConfigChange_SET {
			return unsupportedConfigOp(change, path, "SET or UNSET")
		}
		if err := validateCodeUnitPath(sourceRoot, value, true); err != nil {
			return fmt.Errorf("%q accepts an existing code-unit-relative directory: %w", path, err)
		}
	default:
		return fmt.Errorf("unsupported config path %q (see GetAgentInformation configuration_details)", path)
	}
	return nil
}

func supportedProvisioningKey(key string) bool {
	switch key {
	case "python", "exclude_newer", "editable", "no_project", "requirements", "with", "dependency_groups", "extras", "no_build_isolation", "persistent_venv", "cwd":
		return true
	default:
		return false
	}
}

func splitConfigList(value string) []string {
	var values []string
	for _, item := range strings.Split(value, ",") {
		for _, field := range strings.Fields(item) {
			if field != "" {
				values = append(values, field)
			}
		}
	}
	return values
}

func validatePackageRequirement(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "." || trimmed == ".." || filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../") || (strings.HasPrefix(trimmed, "<") && strings.HasSuffix(trimmed, ">")) {
		return fmt.Errorf("expected a named package requirement, not a path or placeholder")
	}
	parsed, err := piprequirements.New().ParseString(value)
	if err != nil {
		return err
	}
	if len(parsed) != 1 {
		return fmt.Errorf("expected exactly one requirement, parsed %d", len(parsed))
	}
	requirement := parsed[0]
	if strings.TrimSpace(requirement.Name) == "" || requirement.IsFileRef || requirement.IsConstraint || requirement.IsLocalPath || requirement.IsEditable || len(requirement.GlobalOptions) > 0 {
		return fmt.Errorf("expected a named package requirement")
	}
	return nil
}

func validateCodeUnitPath(sourceRoot, relative string, wantDirectory bool) error {
	if strings.TrimSpace(sourceRoot) == "" {
		return fmt.Errorf("builder source root is unavailable")
	}
	clean := filepath.Clean(relative)
	if filepath.IsAbs(relative) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path must stay within the code-unit root")
	}
	root, err := os.OpenRoot(sourceRoot)
	if err != nil {
		return fmt.Errorf("open code-unit root: %w", err)
	}
	defer root.Close()
	info, err := root.Stat(relative)
	if err != nil {
		return err
	}
	if wantDirectory && !info.IsDir() {
		return fmt.Errorf("path is not a directory")
	}
	if !wantDirectory && !info.Mode().IsRegular() {
		return fmt.Errorf("path is not a regular file")
	}
	return nil
}
