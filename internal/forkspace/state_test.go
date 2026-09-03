package forkspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
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
	if err := WriteWorkerState(repo, "perf", WorkerState{Pending: true}); err != nil {
		t.Fatal(err)
	}
	assertForkStatePerm(t, StateDir(repo), 0o700)
	assertForkStatePerm(t, LockPath(repo, "perf"), 0o600)
	assertForkStatePerm(t, PidPath(repo, "perf"), 0o600)
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
	// No pidfile → 0.
	if got := RunningPid(repo, "perf"); got != 0 {
		t.Errorf("RunningPid(no file) = %d, want 0", got)
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
	token := ProcStartToken(os.Getpid())
	if err := WriteWorkerState(repo, "pending-live", WorkerState{Pid: os.Getpid(), Token: token, Pending: true}); err != nil {
		t.Fatal(err)
	}
	if got := RunningPid(repo, "pending-live"); got != os.Getpid() {
		t.Errorf("RunningPid(pending live) = %d, want %d", got, os.Getpid())
	}
	// A dead/out-of-range pid → 0, but its state remains until forkStop reaps any orphaned box.
	if err := os.WriteFile(PidPath(repo, "dead"), []byte(OwnerStateV1+"2147483646\nlinux-proc-v1:1:2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RunningPid(repo, "dead"); got != 0 {
		t.Errorf("RunningPid(dead) = %d, want 0", got)
	}
	if !pathExists(PidPath(repo, "dead")) {
		t.Error("dead worker state must remain as the exact-label reap handle")
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
		{name: "start reservation", raw: OwnerStateV1 + StartClaim + "42\nlinux-proc-v1:boot:123\n", want: WorkerState{Claim: true, Pid: 42, Token: "linux-proc-v1:boot:123"}},
		{name: "start reservation with a launched worker", raw: OwnerStateV1 + StartLaunched + "42\nlinux-proc-v1:boot:123\n", want: WorkerState{Claim: true, Launched: true, Pid: 42, Token: "linux-proc-v1:boot:123"}},
		{name: "reservation without an owner", raw: OwnerStateV1 + StartClaim, bad: true},
		{name: "owner scoped pending", raw: OwnerStateV1 + ReapPending, want: WorkerState{Pending: true}},
		{name: "generation scoped pending", raw: OwnerStateV2 + GenerationTag + string(generation) + "\n" + ReapPending, want: WorkerState{Pending: true, Generation: generation}},
		{name: "headerless pending", raw: ReapPending, bad: true},
		{name: "identified pending", raw: OwnerStateV1 + ReapPending + "42\ndarwin-kinfo-v1:1:2\n", want: WorkerState{Pid: 42, Token: "darwin-kinfo-v1:1:2", Pending: true}},
		{name: "unknown owner version", raw: "owner-v3\n42\ntoken\n", wantIs: ErrUnsupportedWorkerStateVersion},
		{name: "known header missing newline", raw: "owner-v1", bad: true},
		{name: "empty", bad: true},
		{name: "current pending pid one", raw: OwnerStateV1 + ReapPending + "1\ntoken\n", bad: true},
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
	if err := WritePid(repo, "live", os.Getpid()); err != nil {
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

	if err := WritePid(repo, "live", os.Getpid()); err == nil || !strings.Contains(err.Error(), "no stable process identity") {
		t.Fatalf("WritePid without a stable token = %v, want fail-closed error", err)
	}
	if pathExists(PidPath(repo, "live")) {
		t.Fatal("failed identity publication left a pidfile")
	}
}

func TestPublishReservedWorkerRequiresExactReservation(t *testing.T) {
	if !StableProcToken(ProcStartToken(os.Getpid())) {
		t.Skip("kernel process identity unavailable")
	}
	reservation := WorkerState{Claim: true, Launched: true, Pid: os.Getpid(), Token: ProcStartToken(os.Getpid())}
	expected, err := reservation.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("reservation and parent-published identity", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "repo")
		if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(PidPath(repo, "perf"), expected, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := PublishReservedWorker(repo, "perf", expected, os.Getpid()); err != nil {
			t.Fatalf("publish exact reservation: %v", err)
		}
		first, err := os.ReadFile(PidPath(repo, "perf"))
		if err != nil {
			t.Fatal(err)
		}
		if state, err := ParseWorkerState(string(first)); err != nil || state.Claim || state.Pending || state.Pid != os.Getpid() || state.Token != ProcStartToken(os.Getpid()) {
			t.Fatalf("published worker state = (%+v, %v)", state, err)
		}
		if err := PublishReservedWorker(repo, "perf", expected, os.Getpid()); err != nil {
			t.Fatalf("accept parent-published exact worker: %v", err)
		}
		second, err := os.ReadFile(PidPath(repo, "perf"))
		if err != nil || !bytes.Equal(second, first) {
			t.Fatalf("idempotent publication changed state = (%q, %v), want %q", second, err, first)
		}
	})

	tests := []struct {
		name    string
		current []byte
		missing bool
	}{
		{name: "missing", missing: true},
		{name: "pending", current: []byte(OwnerStateV1 + ReapPending)},
		{name: "malformed", current: []byte(OwnerStateV1 + "broken\n")},
		{name: "replaced", current: []byte(OwnerStateV1 + "2147483646\nlinux-proc-v1:1:2\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			if !tc.missing {
				if err := os.WriteFile(PidPath(repo, "perf"), tc.current, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := PublishReservedWorker(repo, "perf", expected, os.Getpid())
			if !errors.Is(err, ErrDetachedStartSuperseded) {
				t.Fatalf("PublishReservedWorker = %v, want superseded", err)
			}
			got, readErr := os.ReadFile(PidPath(repo, "perf"))
			if tc.missing {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("missing state was recreated: %q, %v", got, readErr)
				}
			} else if readErr != nil || !bytes.Equal(got, tc.current) {
				t.Fatalf("denied state changed = (%q, %v), want %q", got, readErr, tc.current)
			}
		})
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
	if err := WritePid(repo, "perf", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	cleared := make(chan struct{})
	go func() {
		ClearPidIfMine(repo, "perf")
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
	if err := WritePid(repo, "mine", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	ClearPidIfMine(repo, "mine")
	if pathExists(PidPath(repo, "mine")) {
		t.Error("ClearPidIfMine should remove a pidfile that names us")
	}
	// A pidfile owned by another pid must be left alone.
	if err := os.WriteFile(PidPath(repo, "other"), []byte(OwnerStateV1+"424242\nlinux-proc-v1:1:2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ClearPidIfMine(repo, "other")
	if !pathExists(PidPath(repo, "other")) {
		t.Error("ClearPidIfMine must NOT remove a pidfile owned by another process")
	}
	if err := WriteWorkerState(repo, "pending", WorkerState{Pid: os.Getpid(), Token: ProcStartToken(os.Getpid()), Pending: true}); err != nil {
		t.Fatal(err)
	}
	ClearPidIfMine(repo, "pending")
	if !pathExists(PidPath(repo, "pending")) {
		t.Error("ClearPidIfMine must leave an in-progress stop marker for forkStop")
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
			ClearPidIfMine(repo, strings.ReplaceAll(tc.name, " ", "-"))
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, tc.raw) {
				t.Fatalf("replacement changed = (%q, %v), want %q", got, err, tc.raw)
			}
		})
	}
}
