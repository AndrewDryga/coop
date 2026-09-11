package scaffold

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
		"portal/nested/.agent/tasks",                   // a member INSIDE a member is a member too
		"portal/vendor/dep/.agent/tasks",               // pruned: build output, even inside a member
		"portal/.agent/hidden/.agent/tasks",            // pruned: a member's own .agent is never walked
		"terraform/environments/production/notamember", // no .agent/ — not a member
	} {
		if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := DetectSubprojects(repo)
	want := []string{"portal", "portal/nested", "terraform/environments/va1"}
	if !slices.Equal(got, want) {
		t.Errorf("DetectSubprojects = %v, want %v", got, want)
	}
}

// The blitz-infra shape: six terraform roots, three of them nested under another root. Discovery
// used to stop at the first member on each path, so the three nested ones were hand-maintained
// forever and every `coop init` printed a count that contradicted the config it would not fix.
func TestDetectSubprojectsFindsMembersNestedInsideAMember(t *testing.T) {
	repo := t.TempDir()
	members := []string{
		"terraform/environments/gcp/blitz",
		"terraform/environments/gcp/immersiveai",
		"terraform/environments/va1",
		"terraform/environments/va1/blitz-apps",
		"terraform/environments/va1/immersive-apps-dev",
		"terraform/environments/va1/immersive-apps-prod",
	}
	for _, m := range members {
		if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(m), ".agent", "tasks"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := DetectSubprojects(repo); !slices.Equal(got, members) {
		t.Errorf("DetectSubprojects = %v, want all six, sorted", got)
	}
	// Registering against a project.yaml that already lists the parents adds only the three
	// nested roots, in place, and a second run adds nothing.
	if _, err := WriteProject(repo, []string{"terraform/environments/gcp/blitz", "terraform/environments/gcp/immersiveai", "terraform/environments/va1"}); err != nil {
		t.Fatal(err)
	}
	added, err := RegisterSubprojects(repo, DetectSubprojects(repo))
	if err != nil {
		t.Fatal(err)
	}
	if want := members[3:]; !slices.Equal(added, want) {
		t.Errorf("RegisterSubprojects added %v, want the nested three %v", added, want)
	}
	if again, err := RegisterSubprojects(repo, DetectSubprojects(repo)); err != nil || len(again) != 0 {
		t.Errorf("second registration added %v (err %v), want nothing", again, err)
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
		for _, keep := range []string{"# Coop project settings.", "#   ports: [5173]", "# gate:"} {
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

func TestWriteProjectNoClobberBoundaries(t *testing.T) {
	t.Run("concurrent creators produce one complete file", func(t *testing.T) {
		repo := t.TempDir()
		const workers = 16
		start := make(chan struct{})
		wrote := make(chan bool, workers)
		errs := make(chan error, workers)
		var wait sync.WaitGroup
		for i := range workers {
			wait.Add(1)
			go func(i int) {
				defer wait.Done()
				<-start
				created, err := WriteProject(repo, []string{fmt.Sprintf("member-%02d", i)})
				wrote <- created
				errs <- err
			}(i)
		}
		close(start)
		wait.Wait()
		close(wrote)
		close(errs)
		created := 0
		for err := range errs {
			if err != nil {
				t.Errorf("concurrent WriteProject: %v", err)
			}
		}
		for result := range wrote {
			if result {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("successful creators = %d, want exactly 1", created)
		}
		pj, err := project.Load(repo)
		if err != nil {
			t.Fatalf("concurrent result is not a complete project file: %v", err)
		}
		if len(pj.Subprojects) != 1 || !strings.HasPrefix(pj.Subprojects[0], "member-") {
			t.Fatalf("concurrent result = %v, want one creator's complete member list", pj.Subprojects)
		}
	})

	for _, tc := range []struct {
		name     string
		dangling bool
	}{
		{name: "final symlink"},
		{name: "dangling final symlink", dangling: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside.yaml")
			if !tc.dangling {
				if err := os.WriteFile(outside, []byte("keep\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			dest := filepath.Join(repo, project.File)
			if err := os.Symlink(outside, dest); err != nil {
				t.Fatal(err)
			}
			if wrote, err := WriteProject(repo, []string{"member"}); err == nil || wrote {
				t.Fatalf("WriteProject = (%v, %v), want symlink refusal", wrote, err)
			}
			if target, err := os.Readlink(dest); err != nil || target != outside {
				t.Fatalf("project symlink changed: target=%q err=%v", target, err)
			}
			if !tc.dangling {
				if got, err := os.ReadFile(outside); err != nil || string(got) != "keep\n" {
					t.Fatalf("outside target changed: %v, %q", err, got)
				}
			}
		})
	}

	t.Run("parent symlink cannot escape repository", func(t *testing.T) {
		repo := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(repo, ".agent")); err != nil {
			t.Fatal(err)
		}
		if wrote, err := WriteProject(repo, []string{"member"}); err == nil || wrote {
			t.Fatalf("WriteProject = (%v, %v), want parent escape refusal", wrote, err)
		}
		if _, err := os.Lstat(filepath.Join(outside, "project.yaml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("scaffold wrote outside repository: %v", err)
		}
	})

	t.Run("partial create is removed", func(t *testing.T) {
		repo := t.TempDir()
		dest := filepath.Join(repo, "partial.txt")
		sentinel := errors.New("interrupted create")
		created, err := writeNewRepoFile(repo, dest, []byte("complete"), 0o644, func(file *os.File, _ []byte) error {
			if _, err := file.Write([]byte("par")); err != nil {
				return err
			}
			return sentinel
		})
		if created || !errors.Is(err, sentinel) {
			t.Fatalf("writeNewRepoFile = (%v, %v), want interrupted failure", created, err)
		}
		if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial scaffold file remains: %v", err)
		}
	})
}

func TestRegisterSubprojectsAtomicReplacement(t *testing.T) {
	t.Run("interrupted stage preserves old file", func(t *testing.T) {
		repo := t.TempDir()
		if _, err := WriteProject(repo, []string{"portal"}); err != nil {
			t.Fatal(err)
		}
		before := readProjectYAML(t, repo)
		sentinel := errors.New("interrupted replacement")
		_, err := registerSubprojects(repo, []string{"portal", "infra"}, func(file *os.File, data []byte) error {
			if _, err := file.Write(data[:len(data)/2]); err != nil {
				return err
			}
			return sentinel
		}, nil)
		if !errors.Is(err, sentinel) {
			t.Fatalf("RegisterSubprojects error = %v, want interrupted replacement", err)
		}
		if got := readProjectYAML(t, repo); got != before {
			t.Fatalf("interrupted replacement changed project.yaml:\n%s", got)
		}
		entries, err := os.ReadDir(filepath.Join(repo, ".agent"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".project.yaml.coop-") {
				t.Fatalf("interrupted replacement left temporary file %s", entry.Name())
			}
		}
	})

	t.Run("concurrent edit is reread and preserved", func(t *testing.T) {
		repo := t.TempDir()
		if _, err := WriteProject(repo, []string{"portal"}); err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		var hookErr error
		hook := func() error {
			once.Do(func() {
				hookErr = os.WriteFile(filepath.Join(repo, project.File), []byte(projectYAML([]string{"external", "portal"})), 0o644)
			})
			return hookErr
		}
		added, err := registerSubprojects(repo, []string{"infra", "portal"}, writeAndSync, hook)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(added, []string{"infra"}) {
			t.Fatalf("added = %v, want only infra after reread", added)
		}
		pj, err := project.Load(repo)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(pj.Subprojects, []string{"external", "infra", "portal"}) {
			t.Fatalf("concurrent edit was lost: %v", pj.Subprojects)
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
