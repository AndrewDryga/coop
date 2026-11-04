package box

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// openSessionDaemon is a Docker that remembers what a session started. The containers exist once the
// test says so, and carry whatever definition the start wrote in its label override.
type openSessionDaemon struct {
	calls, overrides, definition, inspectDB, inspectMigrate, config string
	rt                                                              runtime.Runtime
}

func newOpenSessionDaemon(t *testing.T) *openSessionDaemon {
	t.Helper()
	dir := t.TempDir()
	d := &openSessionDaemon{calls: filepath.Join(dir, "calls"), overrides: filepath.Join(dir, "overrides"),
		definition: filepath.Join(dir, "definition"), inspectDB: filepath.Join(dir, "inspect-db"), inspectMigrate: filepath.Join(dir, "inspect-migrate"), config: filepath.Join(dir, "config")}
	d.resolve(t, migrateOneShot)
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + strconv.Quote(d.calls) + "\n" +
		"prev=\n" +
		"for arg in \"$@\"; do\n" +
		"  case \"$prev:$arg\" in -f:*override-labels*) cat \"$arg\" >> " + strconv.Quote(d.overrides) + " ;; esac\n" +
		"  prev=$arg\n" +
		"done\n" +
		"def=$(cat " + strconv.Quote(d.definition) + " 2>/dev/null)\n" +
		"case \"$*\" in\n" +
		"  *'config --services'*) printf '%s\\n' migrate db ;;\n" +
		"  *'config --format json'*) cat " + strconv.Quote(d.config) + " ;;\n" +
		"  'ps -q -a'*\"label=" + serviceDefinitionLabel + "=${def:-none}\"*) case \"$*\" in\n" +
		"    *'service=db'*) test -s " + strconv.Quote(d.inspectDB) + " && printf '%s\\n' " + strings.Repeat("a", 64) + " ;;\n" +
		"    *'service=migrate'*) test -s " + strconv.Quote(d.inspectMigrate) + " && printf '%s\\n' " + strings.Repeat("b", 64) + " ;;\n" +
		"  esac ;;\n" +
		"  'inspect --format'*" + strings.Repeat("a", 64) + "*) cat " + strconv.Quote(d.inspectDB) + " ;;\n" +
		"  'inspect --format'*" + strings.Repeat("b", 64) + "*) cat " + strconv.Quote(d.inspectMigrate) + " ;;\n" +
		"esac\n"
	shim := filepath.Join(dir, "rt")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d.rt = runtime.Runtime{Name: shim}
	return d
}

// migrateOneShot is the file as Compose resolves it, with db waiting for migrate to finish.
const migrateOneShot = `{"services":{"db":{"expose":["5432"],"depends_on":{"migrate":{"condition":"service_completed_successfully","required":true}}},"migrate":{}}}`

func (d *openSessionDaemon) resolve(t *testing.T, config string) {
	t.Helper()
	if err := os.WriteFile(d.config, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (d *openSessionDaemon) forgetCalls(t *testing.T) {
	t.Helper()
	if err := os.Remove(d.calls); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func (d *openSessionDaemon) called(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(d.calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

type openSessionContainer struct {
	id, status string
	exit       int
	health     string
	hostPort   int
}

func (d *openSessionDaemon) write(t *testing.T, path, service string, compose string, composeProject string, c openSessionContainer, definition string) {
	t.Helper()
	ports := "{}"
	if c.hostPort != 0 {
		ports = fmt.Sprintf(`{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"%d"}]}`, c.hostPort)
	}
	body := fmt.Sprintf(`{"ID":"%s","Labels":{"%s":"%s","%s":"%s","%s":"%s","%s":"False","%s":"%s"},"Status":"%s","ExitCode":%d,"Running":%t,"Paused":false,"Healthcheck":%t,"Health":"%s","Ports":%s,"Networks":{}}`,
		c.id, composeProjectLabel, composeProject, composeWorkingDirLabel, filepath.Dir(compose), composeServiceLabel, service, composeOneoffLabel, serviceDefinitionLabel, definition,
		c.status, c.exit, c.status == "running", c.health != "", c.health, ports)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The order of an open session in a project with services: its own box starts them and labels them
// with what it started from. A later box then joins beside it, under the shared lock the running box
// holds. It runs no `up`, so it cannot restart a service that box uses, and it does not wait.
func TestAnOpenSessionsLaterBoxJoinsWhatItsFirstBoxStarted(t *testing.T) {
	repo, compose := writeCompose(t, "services:\n  db:\n    image: postgres:18\n    expose: [5432]\n  migrate:\n    image: app:1\n")
	const owner = "supervisor-one"
	session := RunSpec{Image: "i", Repo: repo, Cmd: []string{"true"}, Network: true, Batch: true, Quiet: true, ActivitySource: owner, SupervisorID: owner}
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", AutoUp: true}
	d := newOpenSessionDaemon(t)

	// The session's own box finds nothing, starts everything, and leaves its label on what it started.
	if code, err := Run(cfg, d.rt, session); err != nil || code != 0 {
		t.Fatalf("first box: Run = %d, %v", code, err)
	}
	if calls := d.called(t); !strings.Contains(calls, "up -d --wait") {
		t.Fatalf("the first box started no services:\n%s", calls)
	}
	overrides, err := os.ReadFile(d.overrides)
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(strconv.Quote(serviceDefinitionLabel)+`: "([0-9a-f]{64})"`).FindAllStringSubmatch(string(overrides), -1)
	if len(found) != 2 || found[0][1] != found[1][1] {
		t.Fatalf("each service wants one definition label:\n%s", overrides)
	}
	definition := found[0][1]

	composeProject := ComposeProjectFor(repo, owner)
	running := openSessionContainer{id: strings.Repeat("a", 64), status: "running", health: "healthy",
		hostPort: projectHostPort(repo, owner)}
	finished := openSessionContainer{id: strings.Repeat("b", 64), status: "exited"}
	arrange := func(t *testing.T, db, migrate openSessionContainer) {
		t.Helper()
		if err := os.WriteFile(d.definition, []byte(definition), 0o600); err != nil {
			t.Fatal(err)
		}
		d.write(t, d.inspectDB, "db", compose, composeProject, db, definition)
		d.write(t, d.inspectMigrate, "migrate", compose, composeProject, migrate, definition)
		d.forgetCalls(t)
	}
	// The running box's hold: only a box that never asks for the exclusive side gets past it.
	release := holdLaunchLock(t, repo)
	defer release()

	t.Run("beside the running box", func(t *testing.T) {
		arrange(t, running, finished)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		spec := session
		spec.Ctx = ctx
		if code, err := Run(cfg, d.rt, spec); err != nil || code != 0 {
			t.Fatalf("later box: Run = %d, %v", code, err)
		}
		calls := d.called(t)
		if strings.Contains(calls, " up ") || strings.Contains(calls, "--force-recreate") {
			t.Fatalf("a later box touched the running services:\n%s", calls)
		}
		if !strings.Contains(calls, "COOP_SERVICE_DB_URL=") || !strings.Contains(calls, "COOP_FORWARD=") {
			t.Fatalf("a later box was not told where its service is:\n%s", calls)
		}
	})

	// A step that says it never restarts is as finished as one another service waits for.
	t.Run("beside a step that never restarts", func(t *testing.T) {
		arrange(t, running, finished)
		d.resolve(t, `{"services":{"db":{"expose":["5432"]},"migrate":{"restart":"no"}}}`)
		t.Cleanup(func() { d.resolve(t, migrateOneShot) })
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		spec := session
		spec.Ctx = ctx
		if code, err := Run(cfg, d.rt, spec); err != nil || code != 0 {
			t.Fatalf("later box: Run = %d, %v", code, err)
		}
		if calls := d.called(t); strings.Contains(calls, " up ") {
			t.Fatalf("a later box touched the running services:\n%s", calls)
		}
	})

	// Anything but what the session started has to go the whole way, and that way waits for the lock
	// the running box holds: it never reaches `up` while a box uses the services.
	stopped := finished
	stopped.exit = 137
	sick := running
	sick.health = "unhealthy"
	moved := running
	moved.hostPort++
	for _, tc := range []struct {
		name        string
		db, migrate openSessionContainer
		spec        RunSpec
		editFile    bool
		config      string
	}{
		{name: "a service that stopped cleanly", db: running, migrate: finished, spec: session,
			config: `{"services":{"db":{"expose":["5432"]},"migrate":{}}}`},
		{name: "a service waited on only until it starts", db: running, migrate: finished, spec: session,
			config: `{"services":{"db":{"expose":["5432"],"depends_on":{"migrate":{"condition":"service_started","required":true}}},"migrate":{}}}`},
		{name: "a service that restarts on failure", db: running, migrate: finished, spec: session,
			config: `{"services":{"db":{"expose":["5432"]},"migrate":{"restart":"on-failure"}}}`},
		{name: "a service that went down", db: stopped, migrate: finished, spec: session},
		{name: "a service that is not healthy", db: sick, migrate: finished, spec: session},
		{name: "a one-shot step that failed", db: running, migrate: stopped, spec: session},
		{name: "a port it did not get", db: moved, migrate: finished, spec: session},
		{name: "a file edited since", db: running, migrate: finished, spec: session, editFile: true},
		{name: "not a session", db: running, migrate: finished, spec: RunSpec{Image: "i", Repo: repo, Cmd: []string{"true"}, Network: true, Batch: true, Quiet: true, ActivitySource: owner}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrange(t, tc.db, tc.migrate)
			if tc.config != "" {
				d.resolve(t, tc.config)
				t.Cleanup(func() { d.resolve(t, migrateOneShot) })
			}
			if tc.editFile {
				original, err := os.ReadFile(compose)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.WriteFile(compose, original, 0o644) })
				if err := os.WriteFile(compose, append(original, "# edited\n"...), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			spec := tc.spec
			spec.Ctx = ctx
			if _, err := Run(cfg, d.rt, spec); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Run ended with %v, want it still waiting for the lock", err)
			}
			if calls := d.called(t); strings.Contains(calls, " up ") {
				t.Fatalf("services restarted under a running box:\n%s", calls)
			}
		})
	}
}

// Only a session's services carry the definition: on a loop's, a comment edited in the file would
// recreate every one of them at the next `up`.
func TestOnlyASessionsStartLabelsItsDefinition(t *testing.T) {
	repo, _ := writeCompose(t, "services:\n  db:\n    image: postgres:18\n")
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", AutoUp: true}
	d := newOpenSessionDaemon(t)
	spec := RunSpec{Image: "i", Repo: repo, Cmd: []string{"true"}, Network: true, Batch: true, Quiet: true, RunID: "loop-one"}
	if code, err := Run(cfg, d.rt, spec); err != nil || code != 0 {
		t.Fatalf("Run = %d, %v", code, err)
	}
	overrides, err := os.ReadFile(d.overrides)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(overrides), serviceDefinitionLabel) || !strings.Contains(string(overrides), LabelKey) {
		t.Fatalf("a loop's labels:\n%s", overrides)
	}
}

func projectHostPort(repo, owner string) int {
	return project.HostPortFor(servicePortScope(repo, owner), "db:5432")
}
