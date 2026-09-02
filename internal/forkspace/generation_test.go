package forkspace

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func makeGenerationWorkspace(t *testing.T, repo, name string) {
	t.Helper()
	if err := os.MkdirAll(Workspace(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
}

func ensureTestGeneration(t *testing.T, repo, name string) Identity {
	t.Helper()
	unlock, err := LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	identity, err := EnsureGenerationLocked(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestForkGenerationIsStableAndFencesWorkspaceReplacement(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	first := ensureTestGeneration(t, repo, "perf")
	second := ensureTestGeneration(t, repo, "perf")
	if first != second {
		t.Fatalf("resume generation changed: %+v -> %+v", first, second)
	}
	if err := ValidateGenerationWorkspace(repo, first); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(Workspace(repo, "perf")); err != nil {
		t.Fatal(err)
	}
	makeGenerationWorkspace(t, repo, "perf")
	if err := ValidateGenerationWorkspace(repo, first); err == nil {
		t.Fatal("replacement workspace inherited the old generation")
	}

	unlock, err := LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveGenerationIfMatchesLocked(repo, first); err != nil {
		unlock()
		t.Fatal(err)
	}
	replacement, err := EnsureGenerationLocked(repo, "perf")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Generation == first.Generation {
		t.Fatal("reused fork name received the same generation")
	}
}

func TestResolveProjectBindingRequiresExactGenerationWorkspace(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	workspace := Workspace(repo, "perf")
	authority, bound, err := ResolveProjectBinding(workspace)
	if err != nil || authority != repo || bound == nil || *bound != identity {
		t.Fatalf("binding = %q %+v, %v", authority, bound, err)
	}
	plain := filepath.Join(t.TempDir(), "ordinary")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if authority, bound, err := ResolveProjectBinding(plain); err != nil || authority != plain || bound != nil {
		t.Fatalf("ordinary binding = %q %+v, %v", authority, bound, err)
	}
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	makeGenerationWorkspace(t, repo, "perf")
	if _, _, err := ResolveProjectBinding(workspace); err == nil {
		t.Fatal("replacement workspace inherited canonical binding")
	}
}

func TestForkGenerationAdoptsOnlyStoppedLegacyWorkspace(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "legacy")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorkerState(repo, "legacy", WorkerState{Pending: true}); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockState(repo, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGenerationLocked(repo, "legacy"); err == nil {
		unlock()
		t.Fatal("legacy worker state was silently adopted")
	}
	if err := os.Remove(PidPath(repo, "legacy")); err != nil {
		unlock()
		t.Fatal(err)
	}
	if _, err := EnsureGenerationLocked(repo, "legacy"); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
}

func TestForkGenerationRecordFailsClosed(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "bad")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(GenerationPath(repo, "bad"), []byte(`{"version":2,"name":"bad"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadGeneration(repo, "bad"); err == nil {
		t.Fatal("unsupported generation record was treated as absent")
	}
	if err := os.Remove(GenerationPath(repo, "bad")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", GenerationPath(repo, "bad")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadGeneration(repo, "bad"); err == nil {
		t.Fatal("symlinked generation record was accepted")
	}
}

func TestForkGenerationPublicationNeverReplacesExistingAuthority(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	path := GenerationPath(repo, "race")
	want := []byte("existing authority\n")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeGenerationAtomic(repo, "race", []byte("replacement\n")); err == nil {
		t.Fatal("generation publication replaced an existing authority path")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Fatalf("existing generation changed: %q, %v", got, err)
	}
}

func TestForkGenerationPublicationLeavesOneLinkAndNoTemporaryAuthority(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := writeGenerationAtomic(repo, "clean", []byte("authority\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(GenerationPath(repo, "clean"))
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		t.Fatalf("published authority link count = %v", stat)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("published authority mode = %04o, want 0600", got)
	}
	matches, err := filepath.Glob(filepath.Join(StateDir(repo), ".clean.generation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary generation authorities remain: %v", matches)
	}
}
