package scaffold

import (
	"os"
	"path/filepath"
	"slices"
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
	if !slices.Equal(f.services, []string{"db", "redis"}) {
		t.Errorf("services = %v, want [db redis]", f.services)
	}
	// An empty repo finds nothing.
	if detectDocker(t.TempDir()).any() {
		t.Error("empty repo should find no Docker")
	}
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
