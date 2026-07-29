package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Python plugin is a manifest producer only: it renders workload resources
// from normalized inputs and must never learn how that output is transported,
// reviewed, or reconciled. Runtime source therefore may not take ownership of
// Git repositories, GitHub/pull requests, or Argo/Flux reconciliation. This
// guard fails the build if such a dependency ever enters the runtime tree, so
// the emitted bundle stays consumable without Git, network, kubeconfig, Argo,
// or cloud credentials.
//
// Test files are excluded on purpose: fixtures and this guard itself name the
// forbidden concepts as data.
var forbiddenOwnershipTokens = []string{
	"go-git",         // direct Git client / repository operations
	"go-github",      // GitHub API client / pull requests
	"argoproj",       // Argo CD reconciler client
	"fluxcd",         // Flux reconciler client
	"appproject",     // Argo CD AppProject ownership
	"repourl",        // repository source binding
	"targetrevision", // repository revision binding
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
		lower := strings.ToLower(string(content))
		for _, tok := range forbiddenOwnershipTokens {
			if strings.Contains(lower, tok) {
				t.Errorf("%s references transport/reconciler ownership token %q; the plugin must stay manifest-only", path, tok)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
