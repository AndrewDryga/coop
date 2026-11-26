package box

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func fakeServicesUp(t *testing.T, names []string, up bool) *string {
	t.Helper()
	asked := new(string)
	previous := workspaceServicesRunning
	workspaceServicesRunning = func(_ context.Context, _ runtime.Runtime, repo, _ string, _ ...string) (startedServices, bool) {
		*asked = repo
		return startedServices{names: names}, up
	}
	t.Cleanup(func() { workspaceServicesRunning = previous })
	return asked
}

// `coop up` stops waiting only when open boxes hold the lock and the checkout's own stack is wholly up:
// a free lock, a lock only a command holds, and a stack that is not up all keep the wait.
func TestServicesUpBehindBoxesOnlyWhenBoxesHoldTheLockAndTheStackRuns(t *testing.T) {
	repo := t.TempDir()
	asked := fakeServicesUp(t, []string{"db", "keycloak"}, true)
	if _, _, ok := ServicesUpBehindBoxes(t.Context(), runtime.Runtime{}, repo, ""); ok {
		t.Fatal("a free lock skipped the start")
	}
	release := holdLaunchLock(t, repo)
	if _, _, ok := ServicesUpBehindBoxes(t.Context(), runtime.Runtime{}, repo, ""); ok {
		t.Fatal("a lock no box holds skipped the start")
	}
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP})
	boxes, running, ok := ServicesUpBehindBoxes(t.Context(), runtime.Runtime{}, repo, "")
	if !ok || !slices.Equal(boxes, []string{"an editor session"}) || !slices.Equal(running.Names, []string{"db", "keycloak"}) {
		t.Fatalf("an editor session over a running stack = %v, %v, %v", boxes, running, ok)
	}
	if *asked != repo {
		t.Fatalf("asked about %q, want the checkout %q", *asked, repo)
	}
	fakeServicesUp(t, nil, false)
	if _, _, ok := ServicesUpBehindBoxes(t.Context(), runtime.Runtime{}, repo, ""); ok {
		t.Fatal("a stack that is not up skipped the start")
	}
	release()
}

// The real check behind `coop up`'s answer, on the checkout's own project: every service the file
// declares must have its container up, with the port it publishes, or be a one-shot step that finished.
// A missing or stopped service means `up` has work to do. It asks with the labels `coop up` writes, so
// no session definition label.
func TestTheCheckoutsOwnStackRunsOnlyWhenEveryServiceIsUp(t *testing.T) {
	repo, compose := writeCompose(t, "services:\n  db:\n    image: postgres:18\n    expose: [5432]\n  migrate:\n    image: app:1\n")
	d := newOpenSessionDaemon(t)
	d.rt = runtime.Runtime{Name: ownStackShim(t, d)}
	running := openSessionContainer{id: strings.Repeat("a", 64), status: "running", health: "healthy", hostPort: projectHostPort(repo, "")}
	finished := openSessionContainer{id: strings.Repeat("b", 64), status: "exited"}
	for _, c := range []struct {
		name        string
		db, migrate *openSessionContainer
		up          bool
	}{
		{"all up, the one-shot step finished", &running, &finished, true},
		{"db missing", nil, &finished, false},
		{"db stopped", &openSessionContainer{id: running.id, status: "exited", exit: 0, hostPort: running.hostPort}, &finished, false},
		{"db up without its port", &openSessionContainer{id: running.id, status: "running"}, &finished, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			for path, container := range map[string]*openSessionContainer{d.inspectDB: c.db, d.inspectMigrate: c.migrate} {
				if container == nil {
					_ = os.Remove(path)
					continue
				}
				service := "db"
				if path == d.inspectMigrate {
					service = "migrate"
				}
				d.write(t, path, service, compose, ComposeProject(repo), *container, "")
			}
			d.forgetCalls(t)
			started, up := stackRunning(t.Context(), d.rt, repo, compose, "", false)
			if up != c.up {
				t.Fatalf("up = %v, want %v; calls:\n%s", up, c.up, d.called(t))
			}
			calls := d.called(t)
			if strings.Contains(calls, " up ") || strings.Contains(calls, serviceDefinitionLabel) {
				t.Fatalf("the check started services or asked for a session label:\n%s", calls)
			}
			if c.up && (len(started.ports) != 1 || started.ports[0].HostPort != running.hostPort) {
				t.Fatalf("ports = %+v, want db's published port %d", started.ports, running.hostPort)
			}
		})
	}
}

// ownStackShim is the open session daemon's shim without its definition label: `coop up` writes none.
func ownStackShim(t *testing.T, d *openSessionDaemon) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + strconv.Quote(d.calls) + "\n" +
		"case \"$*\" in\n" +
		"  *'config --services'*) printf '%s\\n' migrate db ;;\n" +
		"  *'config --format json'*) cat " + strconv.Quote(d.config) + " ;;\n" +
		"  'ps -q -a'*'service=db'*) test -s " + strconv.Quote(d.inspectDB) + " && printf '%s\\n' " + strings.Repeat("a", 64) + " ;;\n" +
		"  'ps -q -a'*'service=migrate'*) test -s " + strconv.Quote(d.inspectMigrate) + " && printf '%s\\n' " + strings.Repeat("b", 64) + " ;;\n" +
		"  'inspect --format'*" + strings.Repeat("a", 64) + "*) cat " + strconv.Quote(d.inspectDB) + " ;;\n" +
		"  'inspect --format'*" + strings.Repeat("b", 64) + "*) cat " + strconv.Quote(d.inspectMigrate) + " ;;\n" +
		"esac\n"
	path := filepath.Join(t.TempDir(), "rt")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
