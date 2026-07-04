package scaffold

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/project"
)

// TestDetectSubprojectsAndWriteProject: depth-1 dirs with a .agent/ are members (hidden dirs and
// non-projects skipped); WriteProject writes a monorepo root listing them, doesn't clobber, and its
// leaf template parses to an empty project.
func TestDetectSubprojectsAndWriteProject(t *testing.T) {
	repo := t.TempDir()
	for _, m := range []string{"runner", "packs"} {
		if err := os.MkdirAll(filepath.Join(repo, m, ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, "node_modules"), 0o755); err != nil { // no .agent → not a member
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".hidden", ".agent"), 0o755); err != nil { // hidden → skipped
		t.Fatal(err)
	}

	subs := DetectSubprojects(repo)
	if !slices.Equal(subs, []string{"packs", "runner"}) {
		t.Fatalf("DetectSubprojects = %v, want [packs runner]", subs)
	}

	wrote, err := WriteProject(repo, subs)
	if err != nil || !wrote {
		t.Fatalf("WriteProject root: wrote=%v err=%v", wrote, err)
	}
	pj, err := project.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pj.Subprojects, []string{"packs", "runner"}) {
		t.Errorf("written subprojects = %v, want [packs runner]", pj.Subprojects)
	}

	// Idempotent: never clobbers an existing file.
	if w, _ := WriteProject(repo, subs); w {
		t.Error("WriteProject must not overwrite an existing project.yaml")
	}

	// A leaf template (no subprojects) is valid YAML that parses to an empty project.
	leaf := t.TempDir()
	if _, err := WriteProject(leaf, nil); err != nil {
		t.Fatal(err)
	}
	lp, err := project.Load(leaf)
	if err != nil {
		t.Fatalf("leaf template must parse: %v", err)
	}
	if len(lp.Subprojects) != 0 || len(lp.Serve.Ports) != 0 {
		t.Errorf("leaf template should be empty, got %+v", lp)
	}
}
