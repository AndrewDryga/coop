//go:build boxruntimee2e

package box

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/teststate"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// No model is needed to prove that Work can test with its own project toolchain and database.
func TestRuntimeComposeControllerProjectCanTestAndRetainItsOwnData(t *testing.T) {
	if os.Getenv("DOCKER_CONFIG") == "" {
		t.Setenv("DOCKER_CONFIG", filepath.Join(teststate.DeveloperHome(), ".docker"))
	}
	rt, err := runtime.Detect("docker")
	if err != nil {
		t.Fatal(err)
	}
	const image = "postgres:18.6-alpine"
	if code, err := rt.Run(nil, nil, os.Stderr, "pull", image); err != nil || code != 0 {
		t.Fatalf("pull PostgreSQL: %d %v", code, err)
	}
	cfg := &config.Config{BaseImage: image, ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", CPUs: "1", Memory: "1073741824", Pids: "256"}
	compose := `services:
  db:
    image: postgres:18.6-alpine
    environment:
      POSTGRES_PASSWORD: test
    healthcheck:
      test: ["CMD", "pg_isready", "-U", "postgres"]
      interval: 200ms
      timeout: 2s
      retries: 50
    volumes: ["data:/var/lib/postgresql"]
    networks:
      default:
        aliases: [project-db]
networks:
  default: {}
volumes:
  data: {}
`
	newProject := func() string {
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, "dev"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "dev/services.yml"), []byte(compose), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\nRUN touch /project-toolchain-ready\n"), 0644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := StopControllerServices(ctx, rt, repo, true); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		})
		return repo
	}
	first, second, review := newProject(), newProject(), newProject()
	if err := os.MkdirAll(filepath.Join(review, ".agent"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(review, ".agent/project.yaml"), []byte("review:\n  compose: dev/review.yml\n  env:\n    PGHOST: review-db\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(review, "dev/review.yml"), []byte(strings.ReplaceAll(strings.Replace(compose, "  db:", "  review-db:", 1), "project-db", "review-alias")), 0644); err != nil {
		t.Fatal(err)
	}

	run := func(repo, sql string, checking bool) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var output bytes.Buffer
		grant := &ControllerRuntime{Version: 1, JobDigest: strings.Repeat("a", 64), Repository: repo, Runtime: &workerproto.JobRuntime{Version: 1, Dockerfile: "Dockerfile", Compose: "dev/services.yml", Environment: map[string]string{"PGHOST": "project-db", "PGPASSWORD": "test"}}}
		code, err := Run(cfg, rt, RunSpec{Image: image, Repo: repo, Workdir: "/workspace", ControllerJob: true, ControllerRuntime: grant, Review: checking, Batch: true, Quiet: true, Ctx: ctx, Stdout: &output, Stderr: os.Stderr, Cmd: []string{"sh", "-c", "test -f /project-toolchain-ready && psql -U postgres -At -v ON_ERROR_STOP=1 -c \"" + sql + "\""}})
		if err != nil || code != 0 {
			t.Fatalf("project test (review=%v): %d %v; %s", checking, code, err, output.String())
		}
		return strings.TrimSpace(output.String())
	}
	run(first, "CREATE TABLE custody(value text); INSERT INTO custody VALUES ('first-job');", false)
	if err := StopControllerServices(t.Context(), rt, first, false); err != nil {
		t.Fatal(err)
	}
	if got := run(first, "SELECT value FROM custody;", false); got != "first-job" {
		t.Fatalf("second turn lost job data: %q", got)
	}
	if got := run(second, "SELECT count(*) FROM pg_tables WHERE tablename='custody';", false); got != "0" {
		t.Fatalf("another job saw first job data: %q", got)
	}
	if got := run(review, "SELECT count(*) FROM pg_tables WHERE tablename='custody';", true); got != "0" {
		t.Fatalf("review reused Work data: %q", got)
	}
	// A completed review has no runtime left, including its volume.
	assertControllerRuntimeGone(t, rt, review)
	if err := os.RemoveAll(filepath.Join(first, "dev")); err != nil {
		t.Fatal(err)
	}
	if err := StopControllerServices(t.Context(), rt, first, true); err != nil {
		t.Fatal(err)
	}
	assertControllerRuntimeGone(t, rt, first)
	if got := run(second, "SELECT 42;", false); got != "42" {
		t.Fatalf("cleanup touched another job: %q", got)
	}
}

func assertControllerRuntimeGone(t *testing.T, rt runtime.Runtime, repo string) {
	t.Helper()
	for _, resource := range []string{"container", "network", "volume"} {
		var output bytes.Buffer
		code, err := rt.Run(nil, &output, os.Stderr, resource, "ls", "-q", "--filter", "label="+composeProjectLabel+"="+ComposeProject(repo))
		if err != nil || code != 0 || strings.TrimSpace(output.String()) != "" {
			t.Fatalf("%s resources remain for %s: %d %v %s", resource, fmt.Sprintf("%q", repo), code, err, output.String())
		}
	}
}
