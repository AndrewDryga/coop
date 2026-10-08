package forkspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolatedSetupMaterializesTwoLevelNativeSubmodules(t *testing.T) {
	repo := committedSetupRepo(t)
	leaf := committedSetupRepo(t)
	middle := committedSetupRepo(t)
	gitIn(t, middle, "-c", "protocol.file.allow=always", "submodule", "add", "-q", leaf, "leaf")
	gitIn(t, middle, "commit", "-qam", "middle")
	gitIn(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", middle, "middle")
	gitIn(t, repo, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
	gitIn(t, repo, "commit", "-qam", "nested")
	commit, err := gitOutputContext(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := SetupIsolatedContext(t.Context(), repo, "independent", commit)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"middle", "middle/leaf"} {
		source, child := filepath.Join(repo, path), filepath.Join(ws, path)
		if err := ValidateIndependentGit(child); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(child, "README.md"))
		if err != nil || string(data) != "hi\n" {
			t.Fatalf("nested content %q: %q, %v", path, data, err)
		}
		if origin, err := gitOutputContext(t.Context(), child, "config", "--get", "remote.origin.url"); err == nil || origin != "" {
			t.Fatalf("child retained source locator: %q, %v", origin, err)
		}
		roots, err := GitMetadataDirectories(source)
		if err != nil {
			t.Fatal(err)
		}
		parentConfig, err := os.ReadFile(filepath.Join(roots[0], "config"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(child, ".git", "config"), []byte("[core]\n bare=false\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(roots[0], "config"))
		if err != nil || string(got) != string(parentConfig) {
			t.Fatal("child metadata mutation reached trusted parent", err)
		}
	}
}

func TestIsolatedSetupNeverRunsGlobalCheckoutFilters(t *testing.T) {
	repo := committedSetupRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("README.md filter=hostile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".gitattributes")
	gitIn(t, repo, "commit", "-qm", "attributes")
	commit, err := gitOutputContext(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "filter-ran")
	gitIn(t, repo, "config", "--global", "filter.hostile.smudge", "touch '"+marker+"'; cat")
	if _, err := SetupIsolatedContext(context.Background(), repo, "no-filter", commit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("isolated checkout ran the host filter", err)
	}
	// A real unsafe control proves the configured filter and marker work.
	if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "checkout", "--", "README.md")
	if _, err := os.Lstat(marker); err != nil {
		t.Fatal("unsafe positive control did not run", err)
	}
}

func TestIsolatedSetupHydratesOfflineLFSAndRefusesMissingPayloads(t *testing.T) {
	source, commit, files := lfsFixture(t)
	ws, err := SetupIsolatedContext(t.Context(), source, "payloads", commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyLFS(t.Context(), ws, commit); err != nil {
		t.Fatal(err)
	}
	for path, expected := range files {
		actual, err := os.ReadFile(filepath.Join(ws, path))
		if err != nil || string(actual) != string(expected) {
			t.Fatalf("payload %q differs: %v", path, err)
		}
	}
	objects := filepath.Join(source, ".git", "lfs", "objects")
	// Only the owned fixture's payloads are removed; no network fallback is authorized.
	if err := os.RemoveAll(objects); err != nil {
		t.Fatal(err)
	}
	missing, err := SetupIsolatedContext(t.Context(), source, "missing-payloads", commit)
	if err == nil {
		t.Fatal("created an isolated execution tree without its pinned LFS payloads")
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("missing-payload creation retained incomplete workspace", err)
	}
}

func TestIsolatedMaterializationRefusesChangedModulesAndUninitializedSources(t *testing.T) {
	repo := committedSetupRepo(t)
	leaf := committedSetupRepo(t)
	gitIn(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", leaf, "leaf")
	gitIn(t, repo, "commit", "-qam", "submodule")
	base, err := gitOutputContext(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := SetupPinnedContext(t.Context(), repo, "changed", base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".gitmodules"), []byte("[submodule \"leaf\"]\n path=leaf\n url=https://never-fetch.invalid/leaf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, ws, "add", ".gitmodules")
	gitIn(t, ws, "commit", "-qm", "changed-url")
	head, err := gitOutputContext(t.Context(), ws, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := MaterializeIndependentTree(t.Context(), repo, ws, base, head); err == nil || !strings.Contains(err.Error(), ".gitmodules") {
		t.Fatalf("changed metadata = %v", err)
	}
	// An exact metadata file does not authorize a different gitlink commit.
	gitIn(t, ws, "checkout", base, "--", ".gitmodules")
	gitIn(t, leaf, "commit", "--allow-empty", "-qm", "different child")
	childHead, err := gitOutputContext(t.Context(), leaf, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, ws, "update-index", "--cacheinfo", "160000,"+childHead+",leaf")
	gitIn(t, ws, "commit", "-qm", "changed gitlink")
	head, err = gitOutputContext(t.Context(), ws, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := MaterializeIndependentTree(t.Context(), repo, ws, base, head); err == nil || !strings.Contains(err.Error(), "changed submodules") {
		t.Fatalf("changed gitlink = %v", err)
	}
	gitIn(t, repo, "submodule", "deinit", "-f", "leaf")
	missing, err := SetupIsolatedContext(t.Context(), repo, "missing", base)
	if err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("uninitialized source = %v", err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("failed creation retained incomplete workspace", err)
	}
}
