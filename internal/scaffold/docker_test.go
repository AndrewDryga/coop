package scaffold

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestSuggestDocker(t *testing.T) {
	capture := func(repo string) string {
		out, _ := captureScaffoldStderr(t, func() error {
			SuggestDocker(repo)
			return nil
		})
		return out
	}

	// A Dockerized repo with no .agent/Dockerfile → a suggestion naming the agent layer + services.
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("services:\n  db:\n    image: postgres\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := capture(repo)
	for _, want := range []string{"base the agent box", "@anthropic-ai/claude-code", "db", ".agent/compose.yml"} {
		if !strings.Contains(out, want) {
			t.Errorf("suggestion missing %q:\n%s", want, out)
		}
	}

	// A repo that already has .agent/Dockerfile → no nagging.
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM debian\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := capture(repo); got != "" {
		t.Errorf("should not suggest when .agent/Dockerfile exists:\n%s", got)
	}

	// A repo with no Docker → nothing.
	if got := capture(t.TempDir()); got != "" {
		t.Errorf("no-Docker repo should print nothing:\n%s", got)
	}
}
