package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The Python plugin is a manifest producer only: it renders workload resources
// from normalized inputs and must never learn how that output is transported,
// reviewed, or reconciled. Runtime source therefore may not take ownership of
// Git repositories, GitHub/pull requests, or Argo/Flux reconciliation. The
// guard below fails the build if such a dependency ever enters the runtime
// tree, so the emitted bundle stays consumable without Git, network,
// kubeconfig, Argo, or cloud credentials.
//
// Tokens are matched at word boundaries, not as raw substrings, so a concept
// only trips on a delimited occurrence (an import path segment, an identifier,
// a struct field, a manifest key) and never on a longer unrelated name — e.g.
// go-github never reports as go-git, and repoURL never fires inside
// myRepoURLValue.
//
// Test files are excluded on purpose: fixtures and this guard itself name the
// forbidden concepts as data.
var forbiddenOwnership = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"go-git", regexp.MustCompile(`(?i)\bgo-git\b`)},                 // direct Git client / repository operations
	{"go-github", regexp.MustCompile(`(?i)\bgo-github\b`)},           // GitHub API client / pull requests
	{"argoproj", regexp.MustCompile(`(?i)\bargoproj\b`)},             // Argo CD reconciler client
	{"fluxcd", regexp.MustCompile(`(?i)\bfluxcd\b`)},                 // Flux reconciler client
	{"AppProject", regexp.MustCompile(`(?i)\bappproject\b`)},         // Argo CD AppProject ownership
	{"repoURL", regexp.MustCompile(`(?i)\brepourl\b`)},               // repository source binding
	{"targetRevision", regexp.MustCompile(`(?i)\btargetrevision\b`)}, // repository revision binding
}

// scanForbiddenOwnership returns the names of every transport/reconciler
// ownership concept that appears as a delimited token in src, in declaration
// order. It is the single matcher shared by the source walk and its unit test.
func scanForbiddenOwnership(src []byte) []string {
	var hits []string
	for _, f := range forbiddenOwnership {
		if f.pattern.Match(src) {
			hits = append(hits, f.name)
		}
	}
	return hits
}

func TestRuntimeSourceHasNoTransportOwnership(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, tok := range scanForbiddenOwnership(content) {
			t.Errorf("%s references transport/reconciler ownership concept %q; the plugin must stay manifest-only", path, tok)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestScanForbiddenOwnership(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"clean source", "package p\n\nimport \"fmt\"\n", nil},
		{"go-git import", "import \"github.com/go-git/go-git/v5\"", []string{"go-git"}},
		{"go-github is not reported as go-git", "import gh \"github.com/google/go-github/v58/github\"", []string{"go-github"}},
		{"argoproj client import", "import \"github.com/argoproj/argo-cd/v2/pkg/apiclient\"", []string{"argoproj"}},
		{"flux client import", "import \"github.com/fluxcd/source-controller/api/v1\"", []string{"fluxcd"}},
		{"AppProject manifest kind", "const spec = `kind: AppProject`", []string{"AppProject"}},
		{"repoURL source binding", "spec := map[string]string{\"repoURL\": u}", []string{"repoURL"}},
		{"targetRevision binding", "const targetRevision = \"HEAD\"", []string{"targetRevision"}},
		{"token embedded in a longer identifier does not match", "var mapProjectID int\nfunc myRepoURLValue() {}\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scanForbiddenOwnership([]byte(tc.src))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("scanForbiddenOwnership(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}
