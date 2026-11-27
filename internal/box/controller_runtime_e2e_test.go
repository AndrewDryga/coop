//go:build boxruntimee2e

package box

import (
	"bytes"
	"context"
	"crypto/sha256"
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
    expose: ["5432"]
    environment:
      POSTGRES_PASSWORD: test
    healthcheck:
      # TCP, not the socket: while initdb runs, PostgreSQL serves a temporary server on its socket
      # only, and a check that accepted it would call the database ready before it restarts. A cold
      # start took 9.7 s beside a concurrent image build, so it gets a start period.
      test: ["CMD", "pg_isready", "-h", "127.0.0.1", "-U", "postgres"]
      interval: 200ms
      timeout: 2s
      retries: 50
      start_period: 60s
    volumes: ["data:/var/lib/postgresql", "../marker:/job-marker:ro"]
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
		// Runs before the cleanup above: a failed setup keeps its services' own words.
		t.Cleanup(func() {
			if !t.Failed() {
				return
			}
			var ids bytes.Buffer
			_, _ = rt.Run(nil, &ids, os.Stderr, "ps", "-aq", "--filter", "label="+composeProjectLabel+"="+ComposeProject(repo))
			for _, id := range strings.Fields(ids.String()) {
				var logs bytes.Buffer
				_, _ = rt.Run(nil, &logs, &logs, "logs", "--tail", "80", id)
				t.Logf("service %s logs:\n%s", id, logs.String())
			}
		})
		return repo
	}
	source, first, second, review := newProject(), newProject(), newProject(), newProject()
	for _, repo := range []string{first, second, review} {
		if err := os.WriteFile(filepath.Join(repo, "marker"), []byte("workspace-marker"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(repo, "dev/services.yml")); err != nil {
			t.Fatal(err)
		}
	}
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
		grant := &ControllerRuntime{Version: 1, JobDigest: strings.Repeat("a", 64), Repository: source, Runtime: &workerproto.JobRuntime{Version: 1, Dockerfile: "Dockerfile", Compose: "dev/services.yml", Environment: map[string]string{"PGHOST": "project-db", "PGPASSWORD": "test"}}}
		code, err := Run(cfg, rt, RunSpec{Image: image, Repo: repo, Workdir: "/workspace", ControllerJob: true, ControllerRuntime: grant, Review: checking, Batch: true, Quiet: true, Ctx: ctx, Stdout: &output, Stderr: os.Stderr, Cmd: []string{"sh", "-c", "test -f /project-toolchain-ready && psql -U postgres -At -v ON_ERROR_STOP=1 -c \"" + sql + "\""}})
		if err != nil || code != 0 {
			t.Fatalf("project test (review=%v): %d %v; %s", checking, code, err, output.String())
		}
		return strings.TrimSpace(output.String())
	}
	if got := run(first, "SELECT pg_read_file('/job-marker');", false); got != "workspace-marker" {
		t.Fatalf("wrong relative bind: %q", got)
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
	frozen, err := rt.FreezeCompose(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, rawDaemon := frozen.ComposeBinding()
	daemon := fmt.Sprintf("%x", sha256.Sum256([]byte(rawDaemon)))
	observe := func(workspace, digest string) ControllerService {
		t.Helper()
		project := ControllerProject{Workspace: workspace, Repository: source, Definition: "dev/services.yml", JobDigest: digest, Daemon: daemon}
		services, err := project.Services(t.Context(), rt)
		if err != nil || len(services) != 1 {
			t.Fatalf("observe controller project: %+v, %v", services, err)
		}
		return services[0]
	}
	one, two := observe(first, strings.Repeat("a", 64)), observe(second, strings.Repeat("a", 64))
	if one.State != "running" || two.State != "running" || one.ContainerID == two.ContainerID || len(one.Ports) != 1 || one.Ports[0] != 5432 {
		t.Fatalf("independent services did not resolve: %+v, %+v", one, two)
	}
	project := ControllerProject{Workspace: first, Repository: source, Definition: "dev/services.yml", JobDigest: strings.Repeat("a", 64), Daemon: daemon}
	if os.Getenv("COOP_TEST_SERVICE_RELAY") == "1" {
		// Send PostgreSQL's SSLRequest. Even a server that refuses TLS must reply
		// with N before closing the stream. This crosses the real helper image.
		relayCtx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		var reply bytes.Buffer
		finished := false
		err := project.ForwardService(relayCtx, rt, "db", one.ContainerID, 5432,
			bytes.NewReader([]byte{0, 0, 0, 8, 4, 210, 22, 47}), &reply,
			func() error { finished = true; return nil })
		if err != nil || !finished || reply.String() != "N" {
			t.Fatalf("PostgreSQL SSL negotiation through relay: %q, FIN %t, error %v", reply.Bytes(), finished, err)
		}
	}
	logs, err := project.ServiceLogs(t.Context(), rt, "db", one.ContainerID, "")
	if err != nil || len(logs.Records) == 0 || logs.Cursor == "" {
		t.Fatalf("service logs: %+v %v", logs, err)
	}
	if _, err := project.ServiceLogs(t.Context(), rt, "db", one.ContainerID, logs.Cursor); err != nil {
		t.Fatal(err)
	}
	if _, err := project.ServiceLogs(t.Context(), rt, "db", two.ContainerID, ""); err == nil {
		t.Fatal("another project's logs were returned")
	}
	if other := observe(first, strings.Repeat("b", 64)); other.ContainerID != "" {
		t.Fatal("another job selected this service")
	}
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
