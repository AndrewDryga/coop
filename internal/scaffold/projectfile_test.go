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
		for _, keep := range []string{"# Coop project settings.", "#   ports: [5173]", "# gate:", "# gate_sources:"} {
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
		})
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
}

// os.Root follows a link that stays inside the repository, so an in-place edit must refuse a link at
// the file itself: a box could point it at a file .coopignore hides from it, and the edit would copy
// that content into a file the box can read.
func TestInPlaceEditsRefuseLeafLinks(t *testing.T) {
	bodies := map[string]string{"compose.yml": composeOriginal, "project.yaml": "subprojects:\n  - portal\n"}
	for file, body := range bodies {
		// A target in another directory, and one beside the link.
		for _, target := range []string{"../hidden.yml", "hidden.yml"} {
			t.Run(file+" -> "+target, func(t *testing.T) {
				repo := t.TempDir()
				agent := filepath.Join(repo, ".agent")
				if err := os.Mkdir(agent, 0o755); err != nil {
					t.Fatal(err)
				}
				hidden := filepath.Join(agent, filepath.FromSlash(target))
				writeComposeFixture(t, hidden, body)
				link := filepath.Join(agent, file)
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				var err error
				if file == "compose.yml" {
					_, err = AddComposeServices(repo, ".agent/compose.yml", []string{"postgres"})
				} else {
					_, err = RegisterSubprojects(repo, []string{"infra", "portal"})
				}
				if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
					t.Fatalf("edit through a leaf link = %v, want a regular-file refusal", err)
				}
				if got, err := os.Readlink(link); err != nil || got != target {
					t.Fatalf("leaf link replaced: %q, %v", got, err)
				}
				if got := readComposeFixture(t, hidden); got != body {
					t.Fatalf("link target changed: %q", got)
				}
			})
		}
	}
}

// A box can replace .gitignore with a link. Following one out of the repository would append Coop's
// lines to a host file such as ~/.zshrc; following one inside it would edit a file .coopignore may
// hide from the box. Either way the edit is refused and nothing is written.
func TestUpdateGitignoreRefusesLinks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outside bool
	}{
		{name: "link out of the repository", outside: true},
		{name: "link inside the repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			// A link inside the repository is relative: the box's absolute paths are not the host's.
			target, link, want := filepath.Join(repo, ".env"), ".env", []string{".env", ".gitignore"}
			if tc.outside {
				target = filepath.Join(t.TempDir(), ".zshrc")
				link, want = target, []string{".gitignore"}
			}
			const body = "export TOKEN=keep\n"
			if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			gi := filepath.Join(repo, ".gitignore")
			if err := os.Symlink(link, gi); err != nil {
				t.Fatal(err)
			}
			// ErrNotRegular is what makes coop init say "fix the path" rather than "or permissions".
			err := (&scaffolder{repo: repo}).updateGitignore(nil, writeAndSync)
			if !errors.Is(err, ErrNotRegular) || err.Error() != ".gitignore must be a regular file" {
				t.Fatalf("edit through a .gitignore link = %v, want a regular-file refusal matching ErrNotRegular", err)
			}
			if got, err := os.Readlink(gi); err != nil || got != link {
				t.Fatalf(".gitignore link replaced: %q, %v", got, err)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != body {
				t.Fatalf("link target changed: %q, %v", got, err)
			}
			entries, err := os.ReadDir(repo)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if !slices.Equal(names, want) {
				t.Fatalf("repository holds %v after the refusal, want %v", names, want)
			}
		})
	}
}

// The edit is renamed into place, so a .gitignore swapped for a link between the read and the write
// is replaced rather than written through: the bytes the box left in the file never reach the host
// file the link names.
func TestUpdateGitignoreReplacesALinkSwappedInBeforeTheWrite(t *testing.T) {
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), ".zshrc")
	const host = "export PATH=/usr/bin\n"
	if err := os.WriteFile(outside, []byte(host), 0o644); err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(repo, ".gitignore")
	const payload = "node_modules/\n"
	if err := os.WriteFile(gi, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	err := (&scaffolder{repo: repo}).updateGitignore(nil, func(file *os.File, data []byte) error {
		if err := os.Remove(gi); err != nil {
			return err
		}
		if err := os.Symlink(outside, gi); err != nil {
			return err
		}
		return writeAndSync(file, data)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != host {
		t.Fatalf("edit followed the swapped link: %q, %v", got, err)
	}
	if info, err := os.Lstat(gi); err != nil || !info.Mode().IsRegular() {
		t.Fatalf(".gitignore is not the renamed edit: %v, %v", info, err)
	}
	if data, err := os.ReadFile(gi); err != nil || !strings.HasPrefix(string(data), payload) || !strings.Contains(string(data), "\n**/.agent/*\n") {
		t.Fatalf("edit did not land in .gitignore: %v\n%s", err, data)
	}
}

// The read, the staging and the rename share one directory handle, so a parent swapped for a link
// mid-edit cannot carry content read in one directory into another.
func TestRegisterSubprojectsEditStaysWhereItRead(t *testing.T) {
	repo := t.TempDir()
	if _, err := WriteProject(repo, []string{"portal"}); err != nil {
		t.Fatal(err)
	}
	agent, held, decoy := filepath.Join(repo, ".agent"), filepath.Join(repo, ".agent-held"), filepath.Join(repo, "decoy")
	if err := os.Mkdir(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	added, err := registerSubprojects(repo, []string{"infra", "portal"}, func(file *os.File, data []byte) error {
		if err := os.Rename(agent, held); err != nil {
			return err
		}
		if err := os.Symlink("decoy", agent); err != nil {
			return err
		}
		return writeAndSync(file, data)
	})
	if err != nil || !slices.Equal(added, []string{"infra"}) {
		t.Fatalf("registration = %v, %v", added, err)
	}
	if entries, err := os.ReadDir(decoy); err != nil || len(entries) != 0 {
		t.Fatalf("edit followed the swapped link: %v, %v", entries, err)
	}
	if data, err := os.ReadFile(filepath.Join(held, "project.yaml")); err != nil || !strings.Contains(string(data), "  - infra\n") {
		t.Fatalf("edit did not land where it was read: %v\n%s", err, data)
	}
}

func readProjectYAML(t *testing.T, repo string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, ".agent", "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
