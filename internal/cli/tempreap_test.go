package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
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
		orphan: filepath.Join(tmp, "coop-mcp-orphan"), events: filepath.Join(dir, "events"),
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
	orphan := filepath.Join(tmp, "coop-mcp-orphan")
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
	stamp := filepath.Join(dir, "temp-reap")

	reapTemp(fake("broken", "#!/bin/sh\necho 'daemon unavailable' >&2\nexit 1\n"), []string{tmp}, stamp)
	if pathExists(stamp) {
		t.Fatal("a pass that could not list mounts used up the day")
	}
	if !pathExists(orphan) {
		t.Fatal("a pass removed an entry without knowing what is mounted")
	}

	reapTemp(fake("working", "#!/bin/sh\nexit 0\n"), []string{filepath.Join(tmp, "missing"), tmp}, stamp)
	if !pathExists(stamp) {
		t.Fatal("a pass that listed mounts did not count for the day")
	}
	if pathExists(orphan) {
		t.Error("the retried pass left the old, unmounted entry behind")
	}
}

// Both kinds of trigger start the sweep: a launch through runBox, and the orphan-box sweep a loop
// or fork start runs before its boxes, which launch inside the loop engine. Only Main's process
// sweeps, so an app built for one operation never does.
func TestTempReapStartsWithABox(t *testing.T) {
	for name, start := range map[string]func(*testing.T, *app){
		"direct launch": canceledLaunch,
		"loop start":    func(t *testing.T, a *app) { a.collectOrphanBoxes(t.TempDir()) },
	} {
		t.Run(name, func(t *testing.T) {
			stubRecoverNetworkRuns(t, nil, nil)
			f := newTempSweepFixture(t, false)
			start(t, f.app)
			wait.ForFile(t, f.stamp)
			if pathExists(f.orphan) {
				t.Error("the sweep left an old, unmounted coop entry behind")
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
