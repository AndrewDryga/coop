package box

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServicesProject(t *testing.T) {
	cases := map[string]string{
		"/tmp/My_Repo.Name": "coop-my_reponame",
		"/a/b/agent":        "coop-agent",
		"/x/Project 99!":    "coop-project99",
	}
	for in, want := range cases {
		if got := ServicesProject(in); got != want {
			t.Errorf("ServicesProject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImageForRepo(t *testing.T) {
	dir := t.TempDir()
	if got := ImageForRepo(dir, "coop-box", ""); got != "coop-box" {
		t.Errorf("no Dockerfile.agent -> %q, want coop-box", got)
	}
	if got := ImageForRepo(dir, "coop-box", "custom"); got != "custom" {
		t.Errorf("override -> %q, want custom", got)
	}
	os.WriteFile(filepath.Join(dir, "Dockerfile.agent"), []byte("FROM scratch"), 0o644)
	if got := ImageForRepo(dir, "coop-box", ""); got != ServicesProject(dir) {
		t.Errorf("Dockerfile.agent -> %q, want %q", got, ServicesProject(dir))
	}
}

func TestComposeFile(t *testing.T) {
	dir := t.TempDir()
	if ComposeFile(dir) != "" {
		t.Error("no compose file should yield empty string")
	}
	f := filepath.Join(dir, "compose.agent.yml")
	os.WriteFile(f, []byte("services: {}"), 0o644)
	if ComposeFile(dir) != f {
		t.Errorf("ComposeFile = %q, want %q", ComposeFile(dir), f)
	}
	// A zero-byte file counts as none — it declares no services, and coop's own compose decoy
	// can leave an empty mountpoint behind (see composeDecoyStrays).
	os.WriteFile(f, nil, 0o644)
	if ComposeFile(dir) != "" {
		t.Error("an empty compose file should count as no compose file")
	}
}

// The compose decoy shadows every composeFileRels path unconditionally, so Docker creates an
// empty mountpoint (and any parent dir it needs) for an absent one inside the repo bind — a
// stray on the host. box.Run snapshots the absent paths before the run and removeComposeStrays
// clears the empty files, and the empty dirs it caused, after — while sparing a real compose
// file that appeared meanwhile.
func TestComposeDecoyStrayCleanup(t *testing.T) {
	repo := t.TempDir()
	strays := composeDecoyStrays(repo) // nothing exists yet → every compose path is a candidate
	if len(strays) != len(composeFileRels) {
		t.Fatalf("composeDecoyStrays = %d, want %d (one per composeFileRels)", len(strays), len(composeFileRels))
	}
	// Docker would create each as an empty mountpoint (making parent dirs as needed); simulate
	// that, but give the first path real content — it must survive the cleanup.
	real := filepath.Join(repo, filepath.FromSlash(composeFileRels[0]))
	os.WriteFile(real, []byte("services: {}\n"), 0o644)
	for _, rel := range composeFileRels[1:] {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, nil, 0o644)
	}
	removeComposeStrays(repo, strays)
	if _, err := os.Stat(real); err != nil {
		t.Errorf("cleanup deleted a non-empty compose file: %v", err)
	}
	for _, rel := range composeFileRels[1:] {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("empty stray %s not removed (err=%v)", rel, err)
		}
		if d := filepath.Dir(p); d != repo { // the empty parent dir it caused (e.g. .agent/) is pruned too
			if _, err := os.Stat(d); !os.IsNotExist(err) {
				t.Errorf("empty stray dir %s not pruned (err=%v)", d, err)
			}
		}
	}
}

// A directory that predates the run — a real .agent/ with tasks — is never pruned, even though
// the compose stray dropped inside it is removed.
func TestComposeStrayCleanupSparesRealDir(t *testing.T) {
	repo := t.TempDir()
	strays := composeDecoyStrays(repo)
	agentDir := filepath.Join(repo, ".agent")
	os.MkdirAll(agentDir, 0o755)
	os.WriteFile(filepath.Join(agentDir, "keep.txt"), []byte("x"), 0o644)
	for _, rel := range composeFileRels { // Docker drops an empty stray at each path
		p := filepath.Join(repo, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, nil, 0o644)
	}
	removeComposeStrays(repo, strays)
	if _, err := os.Stat(filepath.Join(agentDir, "keep.txt")); err != nil {
		t.Errorf("a real file under .agent/ was deleted: %v", err)
	}
}
