package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
)

func TestParseForkCreateIsolatedIsOptIn(t *testing.T) {
	for _, args := range [][]string{{"safe", "codex", "--isolated"}, {"safe", "frontier", "--isolated", "-d"}, {"safe", "--isolated"}} {
		parsed, err := parseForkCreate(args)
		if err != nil || !parsed.isolated {
			t.Fatalf("parse %v = %+v, %v", args, parsed, err)
		}
	}
	parsed, err := parseForkCreate([]string{"ordinary", "codex"})
	if err != nil || parsed.isolated {
		t.Fatalf("default boundary changed: %+v, %v", parsed, err)
	}
}

func TestIsolatedForkRefusesExistingOrdinaryBeforeLaunch(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(ws, ".git", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkCreate([]string{"ordinary", "codex", "--isolated"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "existing work is unchanged") {
		t.Fatalf("ordinary upgrade = %d, %v", code, err)
	}
	after, err := os.ReadFile(filepath.Join(ws, ".git", "HEAD"))
	if err != nil || string(before) != string(after) {
		t.Fatalf("refusal changed fork HEAD: %q -> %q, %v", before, after, err)
	}
}

func TestRecoverForkBoundaryKeepsIsolatedAdmission(t *testing.T) {
	repo := initRepo(t)
	if err := setupForkBoundary(repo, "safe", true); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "safe")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := bindForkBoundary(repo, "safe", true)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(forkspace.GenerationPath(repo, "safe")); err != nil {
		t.Fatal(err)
	}
	recovered, present, err := recoverForkBoundary(repo, "safe")
	if err != nil || !present || recovered != identity {
		t.Fatalf("recover = %+v, %v, %v", recovered, present, err)
	}
	if isolated, err := forkspace.IsolatedGeneration(repo, recovered); err != nil || !isolated {
		t.Fatalf("recovery downgraded fresh/reopen boundary: %v, %v", isolated, err)
	}
}

func TestIsolatedModeSurvivesMissingWorkspaceBeforeRecreation(t *testing.T) {
	repo := initRepo(t)
	if err := setupForkBoundary(repo, "safe", true); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "safe")
	if err != nil {
		t.Fatal(err)
	}
	_, err = bindForkBoundary(repo, "safe", true)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(forkspace.Workspace(repo, "safe")); err != nil {
		t.Fatal(err)
	}
	for _, fresh := range []bool{false, true} {
		isolated, err := forkBoundaryMode(repo, "safe", false, fresh)
		if err != nil || !isolated {
			t.Fatalf("missing workspace fresh=%v downgraded recreation: %v, %v", fresh, isolated, err)
		}
	}
}
