package forkctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestMergePolicyScansExactFilename(t *testing.T) {
	for _, tc := range []struct {
		name, path, decoy, content, finding string
	}{
		{"secret", "deployment notes.txt", "notes.txt", "\n\n-----BEGIN OPENSSH PRIVATE KEY-----\nfixture\n", "possible secret in deployment notes.txt:3"},
		{"workflow", ".github/workflows/deploy benign.yml", "benign.yml", "name: fixture\n", "Runs on the project's CI runners."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			writeTaskFile(t, filepath.Join(repo, tc.decoy), "benign\n")
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-qm", "benign suffix")
			before := forkspace.GitOut(repo, "rev-parse", "HEAD")
			ws, err := forkspace.Setup(repo, "exact-path")
			if err != nil {
				t.Fatal(err)
			}
			writeTaskFile(t, filepath.Join(ws, tc.path), tc.content)
			git(t, ws, "add", "-A")
			git(t, ws, "commit", "-qm", "changed path")
			if err := gitFetchInto(repo, ws, "exact-path"); err != nil {
				t.Fatal(err)
			}
			warns := strings.Join(mustPolicyScan(t, repo, "review/exact-path"), "\n")
			if !strings.Contains(warns, tc.finding) {
				t.Errorf("scan missed exact path: %q, want %q", warns, tc.finding)
			}
			c := &Control{cfg: &config.Config{}}
			if landed, err := mergeOneForTest(t, c, repo, "", "exact-path", false); landed || err == nil {
				t.Errorf("merge = (%v, %v), want policy refusal", landed, err)
			}
			if got := forkspace.GitOut(repo, "rev-parse", "HEAD"); got != before {
				t.Fatalf("refused merge changed HEAD: %s -> %s", before, got)
			}
		})
	}
}

func TestPolicyAndDossierPreserveGitPaths(t *testing.T) {
	repo := initRepo(t)
	git(t, repo, "checkout", "-qb", "exact-paths")
	paths := []string{"ordinary name.txt", "trailing.txt ", "tab\tname.txt", "line\nname.txt", "quote\"name.txt", "back\\slash.txt", "日本語/AGENTS.md", ".github/workflows/tab\tjob.yml"}
	for _, path := range paths {
		writeTaskFile(t, filepath.Join(repo, path), "ordinary text\n")
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "exact paths")
	git(t, repo, "checkout", "-q", "main")
	if _, err := PolicyScan(repo, "exact-paths"); err != nil {
		t.Fatalf("scan could not read exact Git paths: %v", err)
	}
	surfaces := mustHostSurfaces(t, repo, "exact-paths")
	if len(surfaces) != 1 || surfaces[0].Path != paths[len(paths)-1] {
		t.Errorf("host surfaces = %+v, want exact tab-bearing workflow", surfaces)
	}
	c := &Control{cfg: &config.Config{}}
	out := captureStdout(t, func() {
		if err := c.forkBrief(repo, repo, "exact-paths", "exact-paths", forkReviewGateUnchecked, false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "8 files") || !strings.Contains(out, "A  日本語/AGENTS.md  +1 -0") {
		t.Errorf("dossier lost exact paths/counts: %s", out)
	}
	for _, path := range paths {
		if !strings.Contains(out, "A  "+ui.SafeInline(path)+"  +1 -0") {
			t.Errorf("dossier missing safe exact path %q: %s", path, out)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, paths[0])); !os.IsNotExist(err) {
		t.Fatalf("review changed checkout: %v", err)
	}
}
