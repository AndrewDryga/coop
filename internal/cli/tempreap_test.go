package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// The sweep runs once a day, not once a command.
//
// The leak accumulates over days. Asking the container runtime what is mounted
// on every invocation would put an inspect round trip on a hot path to solve a
// slow problem, and would multiply across parallel coops.
func TestTempReapIsDailyNotPerCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temp-reap")
	now := time.Now()

	if !tempReapDue(path, now) {
		t.Fatal("a machine that has never swept was not due")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if tempReapDue(path, now) {
		t.Error("due again right after a finished sweep")
	}
	if tempReapDue(path, now.Add(23*time.Hour)) {
		t.Error("swept again within the day")
	}
	if !tempReapDue(path, now.Add(25*time.Hour)) {
		t.Error("did not become due after a day")
	}
}

// A short command must leave the day's sweep due.
//
// The sweep runs in the background and dies with its process. When the first command of the day
// stamped the day before sweeping, a quick `coop tasks ls` used the day up and exited mid-sweep,
// and every orphan waited another 24 hours.
func TestShortCommandLeavesTheDailySweepDue(t *testing.T) {
	useEmptyMainConfig(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("COOP_CONFIG_DIR", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("COOP_NO_UPDATE_CHECK", "1")
	// Everything the command could touch is this test's own, down to a runtime that does not exist.
	t.Setenv("COOP_RUNTIME", filepath.Join(t.TempDir(), "missing-runtime"))

	captureStdout(t, func() {
		if code := Main([]string{"credentials"}); code != 0 {
			t.Errorf("coop credentials exit = %d, want 0", code)
		}
	})
	if _, err := os.Stat(filepath.Join(xdg, "coop", "temp-reap")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a short command marked the day's sweep done (stat err %v); it cannot finish one", err)
	}
}

// tempSweepFixture is a host for one sweep: a private temp directory holding an old, unmounted
// coop entry, a Coop home with no stamp, and a container runtime that lists no boxes and records
// its calls. With block set, its `ps` waits for the release file, so a test can look at the stamp
// while the sweep is still running.
type tempSweepFixture struct {
	app                   *app
	orphan, stamp, events string
	started, release      string
}

func newTempSweepFixture(t *testing.T, block bool) tempSweepFixture {
	t.Helper()
	tmp, dir := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", tmp)
	f := tempSweepFixture{
		orphan: filepath.Join(tmp, "coop-audit-trees-orphan"), events: filepath.Join(dir, "events"),
		started: filepath.Join(dir, "started"), release: filepath.Join(dir, "release"),
	}
	if err := os.WriteFile(f.orphan, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(f.orphan, old, old); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "runtime")
	if err := os.WriteFile(cli, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
if [ "$1" = ps ] && [ -n "$COOP_TEST_SWEEP_RELEASE" ]; then
	: > "$COOP_TEST_SWEEP_STARTED"
	while [ ! -e "$COOP_TEST_SWEEP_RELEASE" ]; do sleep 0.01; done
fi
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", f.events)
	t.Setenv("COOP_TEST_SWEEP_STARTED", f.started)
	if block {
		t.Setenv("COOP_TEST_SWEEP_RELEASE", f.release)
	} else {
		t.Setenv("COOP_TEST_SWEEP_RELEASE", "")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // a loop start also settles filtered runs: never the host's records
	f.app = &app{cfg: &config.Config{BoxHome: t.TempDir()}, rt: containerruntime.Runtime{Name: cli}, rtSet: true, tempReapPending: true}
	f.stamp = tempReapPath(f.app.cfg)
	return f
}

// oldDir makes dir, and any missing parent, owner-only and three hours old: an orphan to a sweep.
func oldDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	return dir
}

// canceledLaunch runs a launch that box.Run refuses before any host work, so the sweep it may
// start is all that happens.
func canceledLaunch(t *testing.T, a *app) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.runBox(box.RunSpec{Ctx: ctx}); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("canceled launch = %v, want it refused", err)
	}
}

// The day is stamped when the sweep finishes, not when it starts.
//
// The sweep dies with its process. Stamped at its start, a sweep cut short — a launch that failed
// at once, a coop stopped mid-sweep — still used up the day, and its orphans waited another 24
// hours. Coops started while it runs may sweep as well: that only repeats work, because every pass
// asks the runtime what is mounted first.
func TestTempReapStampsTheDayOnlyOnceItFinishes(t *testing.T) {
	f := newTempSweepFixture(t, true)
	canceledLaunch(t, f.app)

	wait.ForFile(t, f.started)
	if pathExists(f.stamp) {
		t.Fatal("the day was stamped while its sweep was still running; a coop exiting now would lose it")
	}
	if err := os.WriteFile(f.release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wait.ForFile(t, f.stamp)
	if pathExists(f.orphan) {
		t.Error("the finished sweep left an old, unmounted coop entry behind")
	}
	if tempReapDue(f.stamp, time.Now()) {
		t.Error("a finished sweep did not count for the day")
	}

	// One sweep per process, however many boxes it launches.
	canceledLaunch(t, f.app)
	if calls, err := os.ReadFile(f.events); err != nil || strings.Count(string(calls), "ps ") != 1 {
		t.Fatalf("runtime listings = %q (%v), want the one sweep's", calls, err)
	}
}

// A pass that could not ask the runtime what is mounted removed nothing, so it must not use up the
// day: the next launch tries again, at no cost to that launch, since the pass runs in the
// background. A pass that did ask counts even when part of it failed, such as a root it could not
// read, because that would fail again within the day.
func TestTempReapRetriesAPassThatCouldNotListMounts(t *testing.T) {
	tmp, dir := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", tmp)
	// A runfiles root the pass cannot use: a file stands where the folder belongs.
	cfg := &config.Config{BoxHome: dir}
	if err := os.WriteFile(filepath.Join(dir, "runfiles"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(tmp, "coop-audit-trees-orphan")
	if err := os.WriteFile(orphan, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	fake := func(name, script string) containerruntime.Runtime {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return containerruntime.Runtime{Name: path}
	}
	stamp := tempReapPath(cfg)

	reapTemp(fake("broken", "#!/bin/sh\necho 'daemon unavailable' >&2\nexit 1\n"), cfg, stamp)
	if pathExists(stamp) {
		t.Fatal("a pass that could not list mounts used up the day")
	}
	if !pathExists(orphan) {
		t.Fatal("a pass removed an entry without knowing what is mounted")
	}

	// The pass also drops git views whose repository is gone.
	views := t.TempDir()
	t.Setenv(forkspace.GitViewRootEnv, views)
	view := filepath.Join(views, "0123456789abcdef")
	if err := os.Mkdir(view, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone-repo", "objects"), filepath.Join(view, "objects")); err != nil {
		t.Fatal(err)
	}
	reapTemp(fake("working", "#!/bin/sh\nexit 0\n"), cfg, stamp)
	if !pathExists(stamp) {
		t.Fatal("a pass that listed mounts did not count for the day")
	}
	if pathExists(orphan) {
		t.Error("the retried pass left the old, unmounted entry behind")
	}
	if pathExists(view) {
		t.Error("the pass left the view of a repository that is gone")
	}
}

// A pass pins the daemon the current Docker context selects, lists it alone, and removes a killed
// run's folder only when the run recorded that daemon. After `docker context use other` the boxes
// still running on the first daemon are missing from the listing, so their folders stay, as does
// one that names no daemon. A runtime the pass cannot pin, such as one reached over TCP, names no
// daemon: every run folder stays, and the rest is still cleared.
func TestTempReapRemovesOnlyTheListedDaemonsRunFolders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pinned bool
	}{{"pinned", true}, {"cannot be pinned", false}} {
		pinned := tc.pinned
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv(forkspace.GitViewRootEnv, t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("DOCKER_CONTEXT", "")
			endpoint := "tcp://127.0.0.1:2375"
			if pinned {
				endpoint = dockersock.Serve(t, func() (dockersock.Info, error) {
					return dockersock.Info{ID: "daemon-a", OSType: "linux", Architecture: "amd64", ServerVersion: "29", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
				})
			}
			t.Setenv("DOCKER_HOST", endpoint)
			cfg := &config.Config{BoxHome: t.TempDir()}
			run := func(name, daemon string) string {
				dir := filepath.Join(cfg.BoxHome, "runfiles", name)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if daemon != "" {
					if err := os.WriteFile(filepath.Join(dir, "daemon"), []byte(daemon+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return oldDir(t, dir)
			}
			here, elsewhere, unknown := run("coop-run-here", "daemon-a"), run("coop-run-elsewhere", "daemon-b"), run("coop-run-unknown", "")
			client := oldDir(t, filepath.Join(cfg.BoxHome, "runfiles", "coop-docker-client-killed"))
			// Named docker, so the pass pins it. It lists no boxes at all, and records how it was asked.
			calls := filepath.Join(t.TempDir(), "calls")
			docker := filepath.Join(t.TempDir(), "docker")
			if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+strconv.Quote(calls)+"\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			stamp := tempReapPath(cfg)

			reapTemp(containerruntime.Runtime{Name: docker}, cfg, stamp)
			if !pathExists(stamp) || pathExists(client) {
				t.Fatalf("the pass did not finish (stamped: %v, client folder left: %v)", pathExists(stamp), pathExists(client))
			}
			if pathExists(here) == pinned {
				t.Errorf("an old, unmounted run folder of the daemon that answered was removed: %v, want %v", !pathExists(here), pinned)
			}
			if !pathExists(elsewhere) || !pathExists(unknown) {
				t.Errorf("the pass removed a run folder it had no proof for (another daemon's: %v, no daemon's: %v)", !pathExists(elsewhere), !pathExists(unknown))
			}
			listings, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(listings)), "\n") {
				if pinned != strings.Contains(line, "--host "+endpoint+" ") {
					t.Errorf("listing %q went to the pinned daemon: %v, want %v", line, !pinned, pinned)
				}
			}
		})
	}
}

// A host that never staged a run has no runfiles root, and housekeeping must not make one: pinning a
// daemon keeps a Docker client folder in that root while it lists, so the pass pins only when the
// root already exists.
func TestTempReapMakesNoRunfilesRoot(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(forkspace.GitViewRootEnv, t.TempDir())
	t.Setenv("DOCKER_CONTEXT", "")
	// A daemon the pass could pin. Serve also gives the test a config root of its own, where a pin
	// would make the runfiles root.
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "daemon-a", OSType: "linux", Architecture: "amd64", ServerVersion: "29", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
	}))
	// Named docker, so the pass could pin it; it lists no boxes.
	docker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home, err := config.RootDir() // as Load sets BoxHome: the home a pin's client folder goes under
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BoxHome: home}
	stamp := tempReapPath(cfg)

	reapTemp(containerruntime.Runtime{Name: docker}, cfg, stamp)
	if !pathExists(stamp) {
		t.Fatal("the pass did not finish")
	}
	if root := filepath.Join(cfg.BoxHome, "runfiles"); pathExists(root) {
		t.Errorf("housekeeping made the runfiles root %s on a host that never staged a run", root)
	}
}

// Both kinds of trigger start the sweep: a launch through runBox, and the orphan-box sweep a loop
// or fork start runs before its boxes, which launch inside the loop engine. Only Main's process
// sweeps, so an app built for one operation never does.
//
// The sweep clears a killed run's Docker client folder from Coop's private runfiles, and never one
// in the temp directory: in a shared /tmp another user could recreate a live one with a Docker
// config of their own.
func TestTempReapStartsWithABox(t *testing.T) {
	for name, start := range map[string]func(*testing.T, *app){
		"direct launch": canceledLaunch,
		"loop start":    func(t *testing.T, a *app) { a.collectOrphanBoxes(t.TempDir()) },
	} {
		t.Run(name, func(t *testing.T) {
			stubRecoverNetworkRuns(t, nil, nil)
			f := newTempSweepFixture(t, false)
			killed := oldDir(t, filepath.Join(f.app.cfg.BoxHome, "runfiles", "coop-docker-client-killed"))
			earlier := oldDir(t, filepath.Join(filepath.Dir(f.orphan), "coop-docker-client-earlier"))
			start(t, f.app)
			wait.ForFile(t, f.stamp)
			if pathExists(f.orphan) {
				t.Error("the sweep left an old, unmounted coop entry behind")
			}
			if pathExists(killed) {
				t.Error("the sweep left a killed run's Docker client folder in the runfiles")
			}
			if !pathExists(earlier) {
				t.Error("the sweep removed a Docker client folder from the temp directory")
			}
		})
	}

	f := newTempSweepFixture(t, false)
	f.app.tempReapPending = false
	canceledLaunch(t, f.app)
	if pathExists(f.stamp) || !pathExists(f.orphan) {
		t.Fatal("an app built outside Main swept the host's temp entries")
	}
}
