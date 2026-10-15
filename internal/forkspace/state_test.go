package forkspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestForkStateRootAndFilesAreOwnerOnly(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LockPath(repo, "perf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(LockPath(repo, "perf"), 0o644); err != nil {
		t.Fatal(err)
	}

	unlock, err := LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := WriteWorkerState(repo, "perf", WorkerState{Pid: 2147483646, Token: "linux-proc-v1:1:2"}); err != nil {
		t.Fatal(err)
	}
	assertForkStatePerm(t, StateDir(repo), 0o700)
	assertForkStatePerm(t, LockPath(repo, "perf"), 0o600)
	assertForkStatePerm(t, PidPath(repo, "perf"), 0o600)
}

func TestForkStateDirectoryCreationRetryRepeatsParentBarriers(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	previous := syncForkDirectoryEntry
	t.Cleanup(func() { syncForkDirectoryEntry = previous })
	failure := errors.New("synthetic parent directory sync failure")
	syncForkDirectoryEntry = func(*os.File) error { return failure }

	if err := EnsureStateDir(repo); !errors.Is(err, failure) {
		t.Fatalf("first state directory creation = %v, want %v", err, failure)
	}
	if _, err := os.Stat(Home(repo)); err != nil {
		t.Fatalf("fork home was not visible after failed parent barrier: %v", err)
	}

	var synced []string
	syncForkDirectoryEntry = func(dir *os.File) error {
		synced = append(synced, filepath.Clean(dir.Name()))
		return dir.Sync()
	}
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Dir(Home(repo)), Home(repo)} {
		if !slices.Contains(synced, filepath.Clean(want)) {
			t.Errorf("retry did not confirm parent %q; synced %v", want, synced)
		}
	}
}

func TestForkStateRootRefusesUnsafeEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "regular file",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("not a directory\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "project")
			if err := os.MkdirAll(Home(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, StateDir(repo))
			if _, err := LockState(repo, "perf"); err == nil {
				t.Fatal("unsafe fork state root was accepted")
			}
		})
	}
}

func assertForkStatePerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func TestForkStatePaths(t *testing.T) {
	repo := "/home/me/proj"
	if got, want := StateDir(repo), "/home/me/proj-forks/.coop"; got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
	if got, want := LogPath(repo, "perf"), "/home/me/proj-forks/.coop/perf.log"; got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
	if got, want := PidPath(repo, "perf"), "/home/me/proj-forks/.coop/perf.pid"; got != want {
		t.Errorf("PidPath = %q, want %q", got, want)
	}
}

func TestForkRunningPid(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	// No pidfile → 0, and nothing holds the fork.
	if got := RunningPid(repo, "perf"); got != 0 {
		t.Errorf("RunningPid(no file) = %d, want 0", got)
	}
	if NeedsStop(repo, "perf") {
		t.Error("a fork without a lifecycle record must not need stop")
	}
	if pid, held := StateOwner(repo, "perf"); pid != 0 || held {
		t.Errorf("StateOwner(no file) = (%d, %v), want (0, false)", pid, held)
	}
	// Unsupported state is retained as lifecycle authority but never decoded into a running PID.
	unsupported := []byte(strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(PidPath(repo, "perf"), unsupported, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RunningPid(repo, "perf"); got != 0 {
		t.Errorf("RunningPid(malformed state) = %d, want 0", got)
	}
	if !NeedsStop(repo, "perf") {
		t.Error("unsupported state must keep the destructive-operation guard held")
	}
	if pid, held := StateOwner(repo, "perf"); pid != 0 || !held {
		t.Errorf("StateOwner(malformed state) = (%d, %v), want (0, true)", pid, held)
	}
	if got, err := os.ReadFile(PidPath(repo, "perf")); err != nil || string(got) != string(unsupported) {
		t.Fatalf("unsupported state changed = %q, %v; want exact %q", got, err, unsupported)
	}
	// A dead/out-of-range pid → 0, but its state remains until forkStop reaps any orphaned box.
	if err := os.WriteFile(PidPath(repo, "dead"), []byte(OwnerStateV1+"2147483646\nlinux-proc-v1:1:2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RunningPid(repo, "dead"); got != 0 {
		t.Errorf("RunningPid(dead) = %d, want 0", got)
	}
	if !NeedsStop(repo, "dead") {
		t.Error("a dead worker's record must keep the destructive-operation guard held")
	}
	if pid, held := StateOwner(repo, "dead"); pid != 2147483646 || held {
		t.Errorf("StateOwner(dead) = (%d, %v), want (2147483646, false)", pid, held)
	}
	if !pathExists(PidPath(repo, "dead")) {
		t.Error("dead worker state must remain as the exact-label reap handle")
	}
	// A record naming this live process: running, held, and still in need of stop.
	if err := WritePid(repo, "live", os.Getpid(), ""); err != nil {
		t.Fatal(err)
	}
	if got := RunningPid(repo, "live"); got != os.Getpid() {
		t.Errorf("RunningPid(live) = %d, want %d", got, os.Getpid())
	}
	if !NeedsStop(repo, "live") {
		t.Error("a live worker must keep the destructive-operation guard held")
	}
	if pid, held := StateOwner(repo, "live"); pid != os.Getpid() || !held {
		t.Errorf("StateOwner(live) = (%d, %v), want (%d, true)", pid, held, os.Getpid())
	}
}

func TestParsePidfileBody(t *testing.T) {
	if pid, tok := parsePidfile("123\n"); pid != 123 || tok != "" {
		t.Errorf("parsePidfile(pid only) = %d,%q want 123,\"\"", pid, tok)
	}
	if pid, tok := parsePidfile("456\nlinux-proc-v1:boot:123\n"); pid != 456 || tok != "linux-proc-v1:boot:123" {
		t.Errorf("parsePidfile(with token) = %d,%q", pid, tok)
	}
	if pid, _ := parsePidfile("nonsense"); pid != 0 {
		t.Errorf("parsePidfile(junk) pid = %d, want 0", pid)
	}
}

func TestForkWorkerStateWireFormat(t *testing.T) {
	generation := Generation("0123456789abcdef0123456789abcdef")
	cases := []struct {
		name   string
		raw    string
		want   WorkerState
		wantIs error
		bad    bool
	}{
		{name: "owner scoped running", raw: OwnerStateV1 + "42\nlinux-proc-v1:boot:123\n", want: WorkerState{Pid: 42, Token: "linux-proc-v1:boot:123"}},
		{name: "generation scoped running", raw: OwnerStateV2 + GenerationTag + string(generation) + "\n42\nlinux-proc-v1:boot:123\n", want: WorkerState{Pid: 42, Token: "linux-proc-v1:boot:123", Generation: generation}},
		{name: "headerless worker", raw: "42\nlinux-proc-v1:boot:123\n", bad: true},
		// Earlier Coop builds also wrote start reservations and cleanup markers. Nothing writes
		// them now, and they never become an identity: stop and start refuse them unchanged.
		{name: "retired start reservation", raw: OwnerStateV1 + "start-claim\n42\nlinux-proc-v1:boot:123\n", bad: true},
		{name: "retired launched reservation", raw: OwnerStateV2 + GenerationTag + string(generation) + "\nstart-launched\n42\nlinux-proc-v1:boot:123\n", bad: true},
		{name: "retired cleanup marker", raw: OwnerStateV2 + GenerationTag + string(generation) + "\nreap-pending\n", bad: true},
		{name: "unknown owner version", raw: "owner-v3\n42\ntoken\n", wantIs: ErrUnsupportedWorkerStateVersion},
		{name: "known header missing newline", raw: "owner-v1", bad: true},
		{name: "empty", bad: true},
		{name: "pid one", raw: OwnerStateV1 + "1\ntoken\n", bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseWorkerState(tc.raw)
			wantErr := tc.bad || tc.wantIs != nil
			if (err != nil) != wantErr {
				t.Fatalf("ParseWorkerState(%q) error = %v, wantErr %v", tc.raw, err, wantErr)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("ParseWorkerState(%q) error = %v, want errors.Is(_, %v)", tc.raw, err, tc.wantIs)
			}
			if wantErr {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseWorkerState(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
			encoded, err := got.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.raw {
				t.Fatalf("Marshal(%+v) = %q, want exact current bytes %q", got, encoded, tc.raw)
			}
			roundTrip, err := ParseWorkerState(string(encoded))
			if err != nil || !reflect.DeepEqual(roundTrip, got) {
				t.Fatalf("state round trip = %+v, %v; want %+v", roundTrip, err, got)
			}
		})
	}
}

// A pidfile whose stable start-time token no longer matches the pid's process means the pid was
// reused by an unrelated process after the worker crashed → not running, but still cleanup-pending.
// A matching token (a genuinely live worker) is still reported running.
func TestForkRunningPidReusedPid(t *testing.T) {
	if ProcStartToken(os.Getpid()) == "" {
		t.Skip("kernel process identity unavailable — can't test start-time corroboration")
	}
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	// Our own (live) pid, but recorded with a start time from a different process → reused → 0.
	if err := os.WriteFile(PidPath(repo, "reused"), []byte(fmt.Sprintf("%s%d\nlinux-proc-v1:0\n", OwnerStateV1, os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RunningPid(repo, "reused"); got != 0 {
		t.Errorf("RunningPid(reused pid) = %d, want 0", got)
	}
	if !pathExists(PidPath(repo, "reused")) {
		t.Error("reused-pid state must remain for exact-label cleanup")
	}
	// The same pid recorded with its real start time (WritePid round-trip) → genuinely running.
	if err := WritePid(repo, "live", os.Getpid(), ""); err != nil {
		t.Fatal(err)
	}
	if got := RunningPid(repo, "live"); got != os.Getpid() {
		t.Errorf("RunningPid(live, matching token) = %d, want %d", got, os.Getpid())
	}
}

func TestWriteForkPidRejectsMissingStableToken(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	oldRead := ReadProcStartToken
	ReadProcStartToken = func(int) string { return "" }
	t.Cleanup(func() { ReadProcStartToken = oldRead })

	if err := WritePid(repo, "live", os.Getpid(), ""); err == nil || !strings.Contains(err.Error(), "no stable process identity") {
		t.Fatalf("WritePid without a stable token = %v, want fail-closed error", err)
	}
	if pathExists(PidPath(repo, "live")) {
		t.Fatal("failed identity publication left a pidfile")
	}
}

// A detached worker continues only when the record its parent wrote names this exact process, pid
// and start token. Any other record, or none, refuses and leaves what is on disk as it was.
func TestConfirmWorkerAcceptsOnlyItsOwnRecord(t *testing.T) {
	if !StableProcToken(ProcStartToken(os.Getpid())) {
		t.Skip("kernel process identity unavailable")
	}
	generation := Generation("0123456789abcdef0123456789abcdef")
	mine := WorkerState{Pid: os.Getpid(), Token: ProcStartToken(os.Getpid()), Generation: generation}
	for _, tc := range []struct {
		name, raw, wantErr string
	}{
		{name: "own record"},
		{name: "missing", wantErr: "no lifecycle record names this process"},
		{name: "another pid", raw: OwnerStateV2 + GenerationTag + string(generation) + "\n2147483646\nlinux-proc-v1:1:2\n", wantErr: "names another process"},
		{name: "same pid different token", raw: fmt.Sprintf("%s%s%s\n%d\nlinux-proc-v1:replacement:1\n", OwnerStateV2, GenerationTag, generation, os.Getpid()), wantErr: "names another process"},
		{name: "malformed", raw: OwnerStateV1 + "broken\n", wantErr: "invalid detached worker pid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			if tc.wantErr == "" {
				if err := WritePid(repo, "perf", os.Getpid(), generation); err != nil {
					t.Fatal(err)
				}
				if got, err := ConfirmWorker(repo, "perf"); err != nil || got != mine {
					t.Fatalf("ConfirmWorker(own record) = (%+v, %v), want %+v", got, err, mine)
				}
				return
			}
			if err := EnsureStateDir(repo); err != nil {
				t.Fatal(err)
			}
			if tc.raw != "" {
				if err := os.WriteFile(PidPath(repo, "perf"), []byte(tc.raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ConfirmWorker(repo, "perf"); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ConfirmWorker = %v, want a refusal containing %q", err, tc.wantErr)
			}
			got, readErr := os.ReadFile(PidPath(repo, "perf"))
			if tc.raw == "" {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("a refused confirmation created a record: %q, %v", got, readErr)
				}
			} else if readErr != nil || string(got) != tc.raw {
				t.Fatalf("a refused confirmation changed the record = (%q, %v), want %q", got, readErr, tc.raw)
			}
		})
	}
}

// The parent holds the lifecycle lock from before its child starts until the child's record is
// written, so a child that confirms before then waits for the record instead of finding none.
func TestConfirmWorkerWaitsForTheParentsRecord(t *testing.T) {
	if !StableProcToken(ProcStartToken(os.Getpid())) {
		t.Skip("kernel process identity unavailable")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	unlock, err := LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	t.Cleanup(func() { release.Do(unlock) })
	result := make(chan error, 1)
	go func() {
		_, err := ConfirmWorker(repo, "perf")
		result <- err
	}()
	// Observe the confirmation blocked on the held lock rather than inferring it from a delay.
	wait.For(t, "the confirmation to wait on the held lifecycle lock", func() bool {
		select {
		case err := <-result:
			t.Fatalf("confirmation ran without the lifecycle lock: %v", err)
		default:
		}
		buf := make([]byte, 1<<20)
		for _, stack := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			if strings.Contains(stack, "internal/forkspace.ConfirmWorker") && strings.Contains(stack, "internal/forkspace.LockStateContext") {
				return true
			}
		}
		return false
	})
	if err := WritePid(repo, "perf", os.Getpid(), ""); err != nil {
		t.Fatal(err)
	}
	release.Do(unlock)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("confirmation after the parent's record = %v, want it accepted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("confirmation stayed blocked after the lifecycle unlock")
	}
}

func TestProcStartTokenIgnoresCallerTimezoneAndLocale(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	t.Setenv("LC_ALL", "C")
	first := ProcStartToken(os.Getpid())
	t.Setenv("TZ", "Asia/Tokyo")
	t.Setenv("LC_ALL", "POSIX")
	second := ProcStartToken(os.Getpid())
	if first == "" || first != second || !StableProcToken(first) {
		t.Fatalf("stable process tokens = %q and %q", first, second)
	}
}

func TestWorkerCleanupDoesNotWaitForStopLock(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := WritePid(repo, "perf", os.Getpid(), ""); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	cleared := make(chan struct{})
	go func() {
		ClearPidIfMine(repo, "perf", "")
		close(cleared)
	}()
	select {
	case <-cleared:
	case <-time.After(2 * time.Second):
		unlock()
		t.Fatal("worker cleanup waited behind stop's lifecycle lock")
	}
	if !pathExists(PidPath(repo, "perf")) {
		unlock()
		t.Fatal("worker cleanup must leave stop's state marker alone while stop owns the lock")
	}
	unlock()
}

// ClearPidIfMine removes the pidfile only when it names THIS process — never one a different
// live worker owns (the orphan-cascade guard).
func TestClearForkPidIfMine(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WritePid(repo, "mine", os.Getpid(), ""); err != nil {
		t.Fatal(err)
	}
	ClearPidIfMine(repo, "mine", "")
	if pathExists(PidPath(repo, "mine")) {
		t.Error("ClearPidIfMine should remove a pidfile that names us")
	}
	// A pidfile owned by another pid must be left alone.
	if err := os.WriteFile(PidPath(repo, "other"), []byte(OwnerStateV1+"424242\nlinux-proc-v1:1:2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ClearPidIfMine(repo, "other", "")
	if !pathExists(PidPath(repo, "other")) {
		t.Error("ClearPidIfMine must NOT remove a pidfile owned by another process")
	}
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{name: "same pid different token", raw: []byte(fmt.Sprintf("%s%d\nlinux-proc-v1:replacement:1\n", OwnerStateV1, os.Getpid()))},
		{name: "malformed", raw: []byte(OwnerStateV1 + "broken\n")},
		{name: "unsupported", raw: []byte("owner-v3\nfuture\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := PidPath(repo, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.WriteFile(path, tc.raw, 0o644); err != nil {
				t.Fatal(err)
			}
			ClearPidIfMine(repo, strings.ReplaceAll(tc.name, " ", "-"), "")
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, tc.raw) {
				t.Fatalf("replacement changed = (%q, %v), want %q", got, err, tc.raw)
			}
		})
	}
}
