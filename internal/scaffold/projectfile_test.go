package scaffold

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// A member is not always a direct child: an infra repo nests its terraform roots. Depth-1-only
// detection meant those layouts hand-maintained .agent/project.yaml forever.
func TestDetectSubprojectsAtAnyDepth(t *testing.T) {
	repo := t.TempDir()
	for _, d := range []string{
		"terraform/environments/va1/.agent/tasks",      // depth 3 — the shape that motivated this
		"portal/.agent/tasks",                          // depth 1 still works
		"node_modules/pkg/.agent/tasks",                // pruned: dependency tree
		"portal/nested/.agent/tasks",                   // pruned: inside a member already
		"terraform/environments/production/notamember", // no .agent/ — not a member
	} {
		if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := DetectSubprojects(repo)
	want := []string{"portal", "terraform/environments/va1"}
	if !slices.Equal(got, want) {
		t.Errorf("DetectSubprojects = %v, want %v", got, want)
	}
}

// A member added after the first init used to be reported and left for you to type in by hand.
// An unlisted member is a queue coop ignores, so init registers it — without destroying the
// commented template around it.
func TestRegisterSubprojectsEditsInPlace(t *testing.T) {
	t.Run("replaces the placeholder", func(t *testing.T) {
		repo := t.TempDir()
		if _, err := WriteProject(repo, nil); err != nil {
			t.Fatal(err)
		}
		added, err := RegisterSubprojects(repo, []string{"terraform/environments/va1"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(added, []string{"terraform/environments/va1"}) {
			t.Errorf("added = %v", added)
		}
		out := readProjectYAML(t, repo)
		if !strings.Contains(out, "subprojects:\n  - terraform/environments/va1\n") {
			t.Errorf("member not registered:\n%s", out)
		}
		if strings.Contains(out, "# subprojects: [api, web]") {
			t.Errorf("placeholder left behind next to the real block:\n%s", out)
		}
		// The rest of the commented template must survive a surgical edit.
		for _, keep := range []string{"# coop project config", "#   ports: [5173]", "# gate:"} {
			if !strings.Contains(out, keep) {
				t.Errorf("edit destroyed %q — project.yaml documents every key:\n%s", keep, out)
			}
		}
	})

	t.Run("appends to an existing block, sorted, and is idempotent", func(t *testing.T) {
		repo := t.TempDir()
		if _, err := WriteProject(repo, []string{"portal"}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := RegisterSubprojects(repo, []string{"portal", "infra", "terraform/environments/va1"}); err != nil {
				t.Fatal(err)
			}
		}
		out := readProjectYAML(t, repo)
		want := "subprojects:\n  - infra\n  - portal\n  - terraform/environments/va1\n"
		if !strings.Contains(out, want) {
			t.Errorf("want sorted, de-duplicated block:\n%s", out)
		}
	})

	t.Run("leaves a hand-restructured file alone", func(t *testing.T) {
		repo := t.TempDir()
		os.MkdirAll(filepath.Join(repo, ".agent"), 0o755)
		custom := "subprojects: [portal]\n" // flow style — coop can't place a line edit safely
		os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(custom), 0o644)
		added, err := RegisterSubprojects(repo, []string{"portal", "infra"})
		if err != nil {
			t.Fatal(err)
		}
		if len(added) != 0 {
			t.Errorf("should not have edited a flow-style list, added %v", added)
		}
		if out := readProjectYAML(t, repo); out != custom {
			t.Errorf("file was rewritten:\n%s", out)
		}
	})
}

func readProjectYAML(t *testing.T, repo string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, ".agent", "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
