package code

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runners "github.com/codefly-dev/core/runners/base"
)

// CallEdge is a call relationship between two functions.
type CallEdge struct {
	CallerID   string `json:"caller_id"`
	CalleeID   string `json:"callee_id"`
	CallerName string `json:"caller_name"`
	CalleeName string `json:"callee_name"`
	CallType   string `json:"call_type"` // "static", "method", "attribute"
	File       string `json:"file"`
	Line       int    `json:"line"`
}

// ImplementsEdge is a class inheritance relationship.
type ImplementsEdge struct {
	TypeID        string `json:"type_id"`
	TypeName      string `json:"type_name"`
	InterfaceID   string `json:"interface_id"`
	InterfaceName string `json:"interface_name"`
}

// CallGraphResult is the full result of Python call graph analysis.
type CallGraphResult struct {
	Calls      []CallEdge       `json:"calls"`
	Implements []ImplementsEdge `json:"implements"`
	Module     string           `json:"module"`
	Error      string           `json:"error,omitempty"`
}

// ComputePythonCallGraph walks the Python source tree with an ast-based
// script and returns all call edges + inheritance edges. Exported so the
// Tooling layer can invoke it without going through the Code.Execute bus.
func (c *Code) ComputePythonCallGraph(ctx context.Context, srcDir string) CallGraphResult {
	script := `
import json, ast, os, sys

workdir = sys.argv[1]
calls = []
implements = []
seen_calls = set()

skip_dirs = {'.git', '__pycache__', 'venv', '.venv', 'node_modules', 'build',
             'dist', '.tox', '.eggs', '.mypy_cache', '.pytest_cache', 'htmlcov',
             '.nox', 'egg-info'}

for root, dirs, files in os.walk(workdir):
    dirs[:] = [d for d in dirs if d not in skip_dirs and not d.startswith('.')]
    for fname in files:
        if not fname.endswith('.py'):
            continue
        fpath = os.path.join(root, fname)
        rel = os.path.relpath(fpath, workdir)
        try:
            source = open(fpath, errors='replace').read()
            tree = ast.parse(source, filename=fpath)
        except:
            continue

        class_map = {}
        for node in ast.walk(tree):
            if isinstance(node, ast.ClassDef):
                for item in ast.iter_child_nodes(node):
                    if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)):
                        class_map[id(item)] = node.name
                for base in node.bases:
                    base_name = ast.unparse(base) if hasattr(ast, 'unparse') else ''
                    if base_name:
                        implements.append({
                            'type_id': f'{rel}:{node.name}',
                            'type_name': node.name,
                            'interface_id': base_name,
                            'interface_name': base_name,
                        })

        for node in ast.walk(tree):
            if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue

            cls = class_map.get(id(node), '')
            if cls:
                caller_id = f'{rel}:{cls}.{node.name}'
                caller_name = f'{cls}.{node.name}'
            else:
                caller_id = f'{rel}:{node.name}'
                caller_name = node.name

            for child in ast.walk(node):
                if not isinstance(child, ast.Call):
                    continue

                callee_name = ''
                call_type = 'static'
                if isinstance(child.func, ast.Name):
                    callee_name = child.func.id
                elif isinstance(child.func, ast.Attribute):
                    callee_name = child.func.attr
                    call_type = 'attribute'
                    if isinstance(child.func.value, ast.Name):
                        callee_name = f'{child.func.value.id}.{child.func.attr}'
                        call_type = 'method'

                if not callee_name or callee_name == caller_name:
                    continue

                key = (caller_id, callee_name)
                if key in seen_calls:
                    continue
                seen_calls.add(key)

                calls.append({
                    'caller_id': caller_id,
                    'callee_id': callee_name,
                    'caller_name': caller_name,
                    'callee_name': callee_name,
                    'call_type': call_type,
                    'file': rel,
                    'line': getattr(child, 'lineno', 0),
                })

calls = calls[:10000]
implements = implements[:2000]

print(json.dumps({'calls': calls, 'implements': implements, 'module': workdir}))
`

	// Prefer the plugin's configured RunnerEnvironment (native/docker/nix)
	// so AST analysis uses the same interpreter as the running service.
	// Pre-Init falls through ResolveStandaloneEnvironment which picks Nix
	// if declared, native otherwise — keeps mode consistency when possible.
	env := c.Service.ActiveEnv
	if env == nil {
		var rctx *basev0.RuntimeContext
		if c.Service.Base != nil && c.Service.Base.Runtime != nil {
			rctx = c.Service.Base.Runtime.RuntimeContext
		}
		env = runners.ResolveStandaloneEnvironment(ctx, srcDir, rctx)
	}
	proc, procErr := env.NewProcess("python3", "-c", script, srcDir)
	if procErr != nil {
		return CallGraphResult{
			Module: srcDir,
			Error:  fmt.Sprintf("python call graph process: %v", procErr),
		}
	}
	var buf bytes.Buffer
	proc.WithOutput(&buf)
	if runErr := proc.Run(ctx); runErr != nil {
		return CallGraphResult{
			Module: srcDir,
			Error:  fmt.Sprintf("python call graph: %v: %s", runErr, buf.String()),
		}
	}
	out := buf.Bytes()

	var result CallGraphResult
	if err := json.Unmarshal(out, &result); err != nil {
		return CallGraphResult{
			Module: srcDir,
			Error:  fmt.Sprintf("parse call graph: %v", err),
		}
	}
	result.Module = srcDir
	return result
}

// handleFindReferences finds all usages of a symbol via grep + AST.
func (c *Code) handleFindReferences(ctx context.Context, req *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	c.EnsureInit()
	srcDir := c.SourceDir()

	refs := req.GetFindReferences()
	if refs == nil {
		return &codev0.CodeResponse{
			Result: &codev0.CodeResponse_FindReferences{
				FindReferences: &codev0.FindReferencesResponse{},
			},
		}, nil
	}

	symbolName, err := symbolAtPosition(srcDir, refs.File, refs.Line, refs.Column)
	if err != nil || symbolName == "" {
		return &codev0.CodeResponse{
			Result: &codev0.CodeResponse_FindReferences{
				FindReferences: &codev0.FindReferencesResponse{},
			},
		}, nil
	}

	locations := c.grepForSymbol(ctx, srcDir, symbolName)

	return &codev0.CodeResponse{
		Result: &codev0.CodeResponse_FindReferences{
			FindReferences: &codev0.FindReferencesResponse{
				Locations: locations,
			},
		},
	}, nil
}

func symbolAtPosition(srcDir, file string, line, col int32) (string, error) {
	absPath := file
	if !filepath.IsAbs(file) {
		absPath = filepath.Join(srcDir, file)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if int(line) < 1 || int(line) > len(lines) {
		return "", fmt.Errorf("line %d out of range", line)
	}
	lineText := lines[line-1]
	if int(col) < 1 || int(col) > len(lineText) {
		col = 1
	}
	start := int(col) - 1
	end := start
	for start > 0 && isIdentChar(lineText[start-1]) {
		start--
	}
	for end < len(lineText) && isIdentChar(lineText[end]) {
		end++
	}
	if start >= end {
		return "", nil
	}
	return lineText[start:end], nil
}

func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

func (c *Code) grepForSymbol(ctx context.Context, srcDir, symbol string) []*codev0.Location {
	// Route grep through the plugin's active RunnerEnvironment so a
	// Docker-mode Python agent searches its own mounted tree. Pre-Init
	// uses ResolveStandaloneEnvironment which honors declared context.
	env := c.Service.ActiveEnv
	if env == nil {
		var rctx *basev0.RuntimeContext
		if c.Service.Base != nil && c.Service.Base.Runtime != nil {
			rctx = c.Service.Base.Runtime.RuntimeContext
		}
		env = runners.ResolveStandaloneEnvironment(ctx, srcDir, rctx)
	}
	proc, procErr := env.NewProcess("grep", "-rn", "--include=*.py", "-w", symbol, srcDir)
	if procErr != nil {
		return nil
	}
	var buf bytes.Buffer
	proc.WithOutput(&buf)
	_ = proc.Run(ctx) // grep returns non-zero on no match; that's fine
	out := buf.Bytes()

	var locations []*codev0.Location
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		filePath := parts[0]
		relPath, _ := filepath.Rel(srcDir, filePath)
		if relPath == "" {
			relPath = filePath
		}

		lineNum := int32(0)
		fmt.Sscanf(parts[1], "%d", &lineNum)

		col := int32(1)
		if len(parts) >= 3 {
			idx := strings.Index(parts[2], symbol)
			if idx >= 0 {
				col = int32(idx + 1)
			}
		}

		locations = append(locations, &codev0.Location{
			File:   relPath,
			Line:   lineNum,
			Column: col,
		})

		if len(locations) >= 100 {
			break
		}
	}
	return locations
}
