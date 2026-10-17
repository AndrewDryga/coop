package scaffold

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestDetectDocker(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", "FROM alpine\n")
	write("docker/Dockerfile.prod", "FROM debian\n")
	write("docker-compose.yml", "services:\n  db:\n    image: postgres\n  redis:\n    image: redis\nvolumes:\n  pgdata:\n")
	// coop's own box files live in the hidden .agent/ dir, which detectDocker never descends —
	// so neither the box Dockerfile nor the sibling-services compose show up as the repo's own.
	write(".agent/Dockerfile", "FROM debian\n")
	write(".agent/compose.yml", "services:\n  x:\n    image: y\n")
	// a skipped dir is not descended.
	write("node_modules/foo/Dockerfile", "FROM node\n")

	f := detectDocker(repo)
	got := append([]string{}, f.dockerfiles...)
	want := []string{"Dockerfile", filepath.Join("docker", "Dockerfile.prod")}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("dockerfiles = %v, want %v", f.dockerfiles, want)
	}
	if !slices.Equal(f.composes, []string{"docker-compose.yml"}) {
		t.Errorf("composes = %v, want [docker-compose.yml]", f.composes)
	}
	// The names come from the file's own services: block, as `coop help` lists them.
	if names, err := ComposeServiceNames(repo, "docker-compose.yml"); err != nil || !slices.Equal(names, []string{"db", "redis"}) {
		t.Errorf("services = %v, %v; want [db redis]", names, err)
	}
	// An empty repo finds nothing.
	if detectDocker(t.TempDir()).any() {
		t.Error("empty repo should find no Docker")
	}
}

// coop init points at the first Dockerfile detection finds, so detection takes folders in name
// order, whatever order the file system lists them in.
func TestDetectDockerFindsInNameOrder(t *testing.T) {
	repo := t.TempDir()
	var want []string
	for _, name := range []string{"api", "batch", "cron", "docs", "edge", "feed", "gate", "hub"} {
		if err := os.Mkdir(filepath.Join(repo, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		want = append(want, filepath.Join(name, "Dockerfile"))
	}
	if got := detectDocker(repo).dockerfiles; !slices.Equal(got, want) {
		t.Errorf("dockerfiles = %v, want them in name order: %v", got, want)
	}
}

// Docker detection lists the project folder, then opens each folder in it to look one level down.
// A box can swap one for a FIFO or a link in between. Opening a FIFO can wait for a writer and a
// link can lead out of the project, so detection skips that folder at once, instead of freezing
// coop init or pointing at a Dockerfile from another project, and still finds the rest.
func TestDetectDockerSkipsAFolderSwappedAfterItsListing(t *testing.T) {
	if repo := os.Getenv("COOP_SCAFFOLD_FIFO_REPO"); repo != "" {
		previous := afterListed
		t.Cleanup(func() { afterListed = previous })
		outside := t.TempDir()
		// Detection goes by name, so every file can hold the same line.
		for dir, files := range map[string][]string{outside: {"Dockerfile"}, repo: {"Dockerfile", "compose.yml", "web/Dockerfile.prod"}} {
			for _, rel := range files {
				path := filepath.Join(dir, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("FROM alpine\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		infra := filepath.Join(repo, "infra")
		for _, swap := range []string{"a FIFO", "a link"} {
			t.Run(swap, func(t *testing.T) {
				if err := os.Mkdir(infra, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(infra) })
				swapped := false
				afterListed = func(dirRel string) {
					if dirRel != "" || swapped {
						return
					}
					swapped = true
					err := os.Remove(infra)
					if err == nil && swap == "a FIFO" {
						err = syscall.Mkfifo(infra, 0o600)
					} else if err == nil {
						err = os.Symlink(outside, infra)
					}
					if err != nil {
						t.Error(err)
					}
				}
				f := detectDocker(repo)
				if !swapped {
					t.Fatal("detection never reported listing the project folder, so nothing was swapped")
				}
				want := []string{"Dockerfile", filepath.Join("web", "Dockerfile.prod")}
				if !slices.Equal(f.dockerfiles, want) || !slices.Equal(f.composes, []string{"compose.yml"}) {
					t.Errorf("detection with infra swapped for %s = %v, %v; want %v, [compose.yml]", swap, f.dockerfiles, f.composes, want)
				}
			})
		}
		return
	}
	runFIFOChild(t, t.TempDir(), "Docker detection with a folder swapped after its listing")
}

// The project folder itself can be where a box writes, as a checkout kept inside another project
// is. Detection opens it as it opens the folders in it: a FIFO there is refused at once instead of
// waited on, and a link there is not followed.
func TestDetectionRefusesAProjectFolderThatIsNotARealFolder(t *testing.T) {
	if dir := os.Getenv("COOP_SCAFFOLD_FIFO_REPO"); dir != "" {
		real, fifo, link := filepath.Join(dir, "real"), filepath.Join(dir, "fifo"), filepath.Join(dir, "link")
		if err := os.MkdirAll(filepath.Join(real, "api", ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(real, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		for _, root := range []string{fifo, link} {
			if f := detectDocker(root); len(f.dockerfiles)+len(f.composes) > 0 {
				t.Errorf("Docker detection read through %s: %v, %v", filepath.Base(root), f.dockerfiles, f.composes)
			}
			if subs, err := DetectSubprojects(root); err == nil {
				t.Errorf("the subproject search read through %s: %v", filepath.Base(root), subs)
			}
		}
		return
	}
	runFIFOChild(t, t.TempDir(), "detection in a project folder that is not a real folder")
}

func TestDetectDockerSetup(t *testing.T) {
	// A Dockerized repo with no .agent/Dockerfile → the two files a person can point coop at.
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("services:\n  db:\n    image: postgres\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setup := DetectDockerSetup(repo)
	if setup == nil || setup.Dockerfile != "Dockerfile" || setup.Compose != "docker-compose.yml" {
		t.Fatalf("detected setup = %+v, want the repo's own Dockerfile and Compose file", setup)
	}

	// A repo that already has .agent/Dockerfile → nothing to suggest.
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM debian\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetectDockerSetup(repo); got != nil {
		t.Errorf("should not suggest when .agent/Dockerfile exists: %+v", got)
	}

	// A repo with no Docker → nothing.
	if got := DetectDockerSetup(t.TempDir()); got != nil {
		t.Errorf("no-Docker repo has nothing to point at: %+v", got)
	}
}
