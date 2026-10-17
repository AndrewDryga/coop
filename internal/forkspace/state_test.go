package forkspace

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
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
	if err := WriteWorkerState(repo, "perf", WorkerState{Pid: 2147483646, Token: starttoken.Stale()}); err != nil {
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
	if err := os.WriteFile(PidPath(repo, "dead"), []byte(OwnerStateV1+"2147483646\n"+starttoken.Stale()+"\n"), 0o644); err != nil {
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

// plantSocket leaves a Unix socket at path. t.TempDir holds the long test name, past Darwin's
// 104-byte socket path limit, so the socket is bound in a short folder and moved into place.
func plantSocket(path string) error {
	dir, err := os.MkdirTemp("", "coop-sock-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	bound := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", bound)
	if err != nil {
		return err
	}
	defer listener.Close()
	return os.Rename(bound, path)
}

// A box whose mount holds a fork's .coop folder can put anything at a lifecycle record's name. Every
// reader of the record refuses what Coop never writes there, at once, and says what it found: none
// waits on a FIFO, reads a device or reads past the limit, and the fork stays held until
// `coop fork stop`.
func TestWorkerStateReadersRefuseWhatIsNotARecord(t *testing.T) {
	for _, tc := range []struct {
		name  string
		found string
		plant func(path string) error
	}{
		{name: "FIFO with no writer", found: "a named pipe", plant: func(path string) error { return syscall.Mkfifo(path, 0o600) }},
		{name: "link to dev zero", found: "a link", plant: func(path string) error { return os.Symlink("/dev/zero", path) }},
		{name: "dangling link", found: "a link", plant: func(path string) error { return os.Symlink(path+".gone", path) }},
		{name: "folder", found: "a folder", plant: func(path string) error { return os.Mkdir(path, 0o700) }},
		{name: "socket", found: "a socket", plant: plantSocket},
		{name: "oversized file", found: fmt.Sprintf("%d bytes, larger than any record", workerStateLimit+1), plant: func(path string) error {
			record := OwnerStateV1 + "2147483646\nlinux-proc-v1:1:2\n"
			return os.WriteFile(path, []byte(record+strings.Repeat(" ", workerStateLimit+1-len(record))), 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "proj")
			if err := EnsureStateDir(repo); err != nil {
				t.Fatal(err)
			}
			path := PidPath(repo, "perf")
			if err := tc.plant(path); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if _, err := ReadWorkerState(repo, "perf"); !errors.Is(err, ErrNotAStateFile) || err.Error() != path+" is "+tc.found {
					t.Errorf("ReadWorkerState = %v, want %s is %s", err, path, tc.found)
				}
				if pid := RunningPid(repo, "perf"); pid != 0 {
					t.Errorf("RunningPid = %d, want 0", pid)
				}
				if pid, held := StateOwner(repo, "perf"); pid != 0 || !held {
					t.Errorf("StateOwner = (%d, %v), want (0, true)", pid, held)
				}
				if !NeedsStop(repo, "perf") {
					t.Error("a record Coop cannot read must keep the destructive-operation guard held")
				}
				if _, err := ConfirmWorker(repo, "perf"); !errors.Is(err, ErrNotAStateFile) {
					t.Errorf("ConfirmWorker = %v, want %s is %s", err, path, tc.found)
				}
				ClearPidIfMine(repo, "perf", "")
			}()
			select {
			case <-done:
			case <-time.After(wait.Deadline):
				t.Fatalf("the readers of a %s did not return within %v", tc.name, wait.Deadline)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() || after.Size() != before.Size() {
				t.Fatalf("the refused record changed: %v, %v", after, err)
			}
		})
	}
}

// A box whose mount holds a fork's .coop folder can plant anything at a lock's or a log's name.
// Taking the lock and opening the log refuse what is not their file at once and say what they
// found; neither follows a link to create, empty or tighten a file outside the forks tree.
func TestLockAndLogRefuseWhatIsNotTheirFile(t *testing.T) {
	for _, w := range []struct {
		name, file string
		quiet      bool // TryLockState reports no reason
		open       func(repo string) error
	}{
		{name: "LockState", file: "perf.lock", open: lockState},
		{name: "TryLockState", file: "perf.lock", quiet: true, open: tryLock},
		{name: "OpenLog", file: "perf.log", open: openLog},
	} {
		for _, p := range []struct {
			name, found string
			plant       func(path, outside string) error
		}{
			{name: "link to a file outside", found: "a link", plant: func(path, outside string) error { return os.Symlink(outside, path) }},
			{name: "link to a new file outside", found: "a link", plant: func(path, outside string) error { return os.Symlink(outside+".new", path) }},
			{name: "FIFO with no reader", found: "a named pipe", plant: func(path, _ string) error { return syscall.Mkfifo(path, 0o600) }},
			{name: "folder", found: "a folder", plant: func(path, _ string) error { return os.Mkdir(path, 0o700) }},
			{name: "socket", found: "a socket", plant: func(path, _ string) error { return plantSocket(path) }},
		} {
			t.Run(w.name+"/"+p.name, func(t *testing.T) {
				repo := filepath.Join(t.TempDir(), "proj")
				if err := EnsureStateDir(repo); err != nil {
					t.Fatal(err)
				}
				outside := filepath.Join(t.TempDir(), "user-file")
				writeModeFile(t, outside, "keep\n")
				untouched := treeState(t, filepath.Dir(outside))
				path := filepath.Join(StateDir(repo), w.file)
				if err := p.plant(path, outside); err != nil {
					t.Fatal(err)
				}
				planted, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- w.open(repo) }()
				select {
				case err = <-done:
				case <-time.After(wait.Deadline):
					t.Fatalf("%s over a %s did not return within %v", w.name, p.name, wait.Deadline)
				}
				if want := path + " is " + p.found; err == nil || (!w.quiet && (!errors.Is(err, ErrNotAStateFile) || err.Error() != want)) {
					t.Fatalf("%s over a %s = %v, want the refusal %s", w.name, p.name, err, want)
				}
				if after, err := os.Lstat(path); err != nil || !os.SameFile(planted, after) || after.Mode() != planted.Mode() || after.Size() != planted.Size() {
					t.Fatalf("the planted %s changed: %v, %v", p.name, after, err)
				}
				if after := treeState(t, filepath.Dir(outside)); !reflect.DeepEqual(after, untouched) {
					t.Fatalf("%s changed files outside the forks tree:\nbefore %v\nafter  %v", w.name, untouched, after)
				}
			})
		}
	}
}

// Each start empties the log and makes it owner-only, whatever mode it had.
func TestOpenLogEmptiesAndTightensTheLog(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	writeModeFile(t, LogPath(repo, "perf"), "old output\n")
	f, err := OpenLog(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("new output\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	assertForkStatePerm(t, LogPath(repo, "perf"), 0o600)
	if got, err := os.ReadFile(LogPath(repo, "perf")); err != nil || string(got) != "new output\n" {
		t.Fatalf("log = %q, %v; want only the new start's output", got, err)
	}
}

// A child whose start died before its record still writes through that start's open of the log.
// Its refusal must land after a newer start's output, not over it.
func TestOpenLogEarlierOpenAppendsAfterANewerStart(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	earlier, err := OpenLog(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	defer earlier.Close()
	newer, err := OpenLog(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Close()
	if _, err := newer.WriteString("newer start output\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := earlier.WriteString("refused\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(LogPath(repo, "perf")); err != nil || string(got) != "newer start output\nrefused\n" {
		t.Fatalf("log = %q, %v; want the refusal after the newer start's output", got, err)
	}
}

// stateWriters is every forkspace call that writes in a fork's .coop folder, and what each leaves in
// the folder it opened. Quiet calls report no reason when they refuse, so only the unchanged files
// show it.
var stateWriters = []struct {
	name  string
	nth   int // which open of .coop is the call's own: ClearPidIfMine opens it to lock first
	quiet bool
	write func(repo string) error
	did   func(t *testing.T, dir string)
}{
	{name: "EnsureStateDir", nth: 1, write: EnsureStateDir, did: func(*testing.T, string) {}},
	{name: "LockState", nth: 1, write: lockState, did: func(t *testing.T, dir string) {
		assertForkStatePerm(t, filepath.Join(dir, "perf.lock"), 0o600)
	}},
	{name: "TryLockState", nth: 1, quiet: true, write: tryLock, did: func(t *testing.T, dir string) {
		assertForkStatePerm(t, filepath.Join(dir, "perf.lock"), 0o600)
	}},
	{name: "OpenLog", nth: 1, write: openLog, did: func(t *testing.T, dir string) {
		assertForkStatePerm(t, filepath.Join(dir, "perf.log"), 0o600)
	}},
	{name: "WriteWorkerState", nth: 1, write: func(repo string) error {
		return WriteWorkerState(repo, "perf", WorkerState{Pid: 2147483646, Token: "linux-proc-v1:1:2"})
	}, did: func(t *testing.T, dir string) {
		if got, err := os.ReadFile(filepath.Join(dir, "perf.pid")); err != nil || string(got) != OwnerStateV1+"2147483646\nlinux-proc-v1:1:2\n" {
			t.Fatalf("record = %q, %v; want the new worker", got, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".perf.pid-") {
				t.Fatalf("the record's temporary file %s was left behind", entry.Name())
			}
		}
	}},
	{name: "RemoveWorkerState", nth: 1, write: func(repo string) error { return RemoveWorkerState(repo, "perf") }, did: assertNoRecord},
	{name: "ClearPidIfMine", nth: 2, quiet: true, write: func(repo string) error {
		ClearPidIfMine(repo, "perf", "")
		return nil
	}, did: assertNoRecord},
}

// A box that can write the forks folder can swap .coop for a link to a folder elsewhere after Coop
// checks it. Every writer works through the handle it opened, so it finishes in the real folder and
// nothing outside the forks tree is created, emptied, replaced, removed or chmodded; the next call
// refuses the link plainly.
func TestStateWritersKeepToTheStateFolderTheyOpened(t *testing.T) {
	for _, w := range stateWriters {
		t.Run(w.name, func(t *testing.T) {
			repo, outside := swapFixture(t, ".")
			if err := os.Chmod(StateDir(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			untouched := treeState(t, outside)
			swapped := swapAtSync(t, Home(repo), w.nth, StateDir(repo), outside)
			if err := w.write(repo); err != nil {
				t.Fatalf("%s across the swap = %v, want it to finish in the folder it opened", w.name, err)
			}
			if !*swapped {
				t.Fatal("the swap never happened")
			}
			if after := treeState(t, outside); !reflect.DeepEqual(after, untouched) {
				t.Fatalf("%s changed files outside the forks tree:\nbefore %v\nafter  %v", w.name, untouched, after)
			}
			opened := StateDir(repo) + ".pinned"
			assertForkStatePerm(t, opened, 0o700)
			w.did(t, opened)
			want := fmt.Sprintf("fork state path %q is not a real directory", StateDir(repo))
			if err := w.write(repo); !w.quiet && (err == nil || err.Error() != want) {
				t.Fatalf("%s through the link = %v, want %s", w.name, err, want)
			}
			if after := treeState(t, outside); !reflect.DeepEqual(after, untouched) {
				t.Fatalf("%s through the link changed files outside the forks tree:\nbefore %v\nafter  %v", w.name, untouched, after)
			}
		})
	}
}

// The same swap one level up, by a box whose mount holds the folder the forks folder sits in: every
// writer refuses, and nothing changes outside the forks tree or in the real .coop.
func TestStateWritersRefuseASwappedForksFolder(t *testing.T) {
	for _, w := range stateWriters {
		t.Run(w.name, func(t *testing.T) {
			repo, outside := swapFixture(t, ".coop")
			untouched, state := treeState(t, outside), treeState(t, StateDir(repo))
			swapped := swapAtSync(t, filepath.Dir(Home(repo)), 1, Home(repo), outside)
			err := w.write(repo)
			if !*swapped {
				t.Fatal("the swap never happened")
			}
			if want := fmt.Sprintf("%s is not a real directory", Home(repo)); !w.quiet && (err == nil || err.Error() != want) {
				t.Fatalf("%s across the swap = %v, want %s", w.name, err, want)
			}
			if after := treeState(t, outside); !reflect.DeepEqual(after, untouched) {
				t.Fatalf("%s changed files outside the forks tree:\nbefore %v\nafter  %v", w.name, untouched, after)
			}
			if after := treeState(t, filepath.Join(Home(repo)+".pinned", ".coop")); !reflect.DeepEqual(after, state) {
				t.Fatalf("%s changed the real .coop:\nbefore %v\nafter  %v", w.name, state, after)
			}
		})
	}
}

func lockState(repo string) error {
	unlock, err := LockState(repo, "perf")
	if err == nil {
		unlock()
	}
	return err
}

func tryLock(repo string) error {
	unlock, ok := TryLockState(repo, "perf")
	if !ok {
		return errors.New("TryLockState did not take the lock")
	}
	unlock()
	return nil
}

func openLog(repo string) error {
	f, err := OpenLog(repo, "perf")
	if err != nil {
		return err
	}
	return f.Close()
}

func assertNoRecord(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, "perf.pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the record is still in the folder the call opened: %v", err)
	}
}

// swapFixture is a fork whose worker record names this process, and a folder outside the forks tree
// holding a file at every name a writer uses, the record's own bytes included, so a writer that
// followed a link there would show. under is where those files sit in outside: "." for a link at
// .coop, ".coop" for a link at the forks folder.
func swapFixture(t *testing.T, under string) (repo, outside string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "proj")
	if err := WritePid(repo, "perf", os.Getpid(), ""); err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(PidPath(repo, "perf"))
	if err != nil {
		t.Fatal(err)
	}
	outside = t.TempDir()
	files := filepath.Join(outside, under)
	if err := os.MkdirAll(files, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(files, 0o755); err != nil {
		t.Fatal(err)
	}
	writeModeFile(t, filepath.Join(files, "perf.lock"), "keep\n")
	writeModeFile(t, filepath.Join(files, "perf.log"), "keep\n")
	writeModeFile(t, filepath.Join(files, "perf.pid"), string(record))
	return repo, outside
}

// writeModeFile writes a 0644 file, whatever the umask.
func writeModeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

// swapAtSync swaps path for a link to target at the nth sync of the folder synced, as a box could
// between Coop's check of that folder and its writes there, and reports whether it did. What was at
// path moves to path+".pinned".
func swapAtSync(t *testing.T, synced string, nth int, path, target string) *bool {
	t.Helper()
	previous := syncForkDirectoryEntry
	t.Cleanup(func() { syncForkDirectoryEntry = previous })
	seen, swapped := 0, false
	syncForkDirectoryEntry = func(dir *os.File) error {
		if filepath.Clean(dir.Name()) == filepath.Clean(synced) {
			if seen++; seen == nth {
				if err := os.Rename(path, path+".pinned"); err != nil {
					return err
				}
				if err := os.Symlink(target, path); err != nil {
					return err
				}
				swapped = true
			}
		}
		return dir.Sync()
	}
	return &swapped
}

// treeState records every entry under root with its type, mode, inode and bytes, so a file created,
// removed, replaced, emptied or chmodded anywhere under it shows as a difference.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		var data []byte
		if info.Mode().IsRegular() {
			if data, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		state[rel] = fmt.Sprintf("%v inode %d %q", info.Mode(), info.Sys().(*syscall.Stat_t).Ino, data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
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
	if err := os.WriteFile(PidPath(repo, "reused"), []byte(fmt.Sprintf("%s%d\n%s\n", OwnerStateV1, os.Getpid(), starttoken.Stale())), 0o644); err != nil {
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

// A record another operating system wrote (a coop in a Linux container sharing this folder with
// macOS, or the reverse) names a pid this kernel cannot check. Whether that pid is missing here or
// held by this very process, the worker is not running as far as status goes, but the fork stays
// held, the record is never provably dead, and no pid here is even asked.
func TestForkRecordFromAnotherSystemStaysHeld(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	oldSignal := SignalPID
	SignalPID = func(pid int, sig syscall.Signal) error {
		t.Errorf("signal %v sent to pid %d for another system's record", sig, pid)
		return nil
	}
	t.Cleanup(func() { SignalPID = oldSignal })
	for _, pid := range []int{2147483646, os.Getpid()} {
		name := "foreign-" + strconv.Itoa(pid)
		state := WorkerState{Pid: pid, Token: starttoken.Foreign()}
		if err := WriteWorkerState(repo, name, state); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(PidPath(repo, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := ProcessIdentityOf(pid, state.Token); got != ProcessIdentityForeign || OwnerProvablyDead(got) {
			t.Errorf("ProcessIdentityOf(%d, %q) = %v, want foreign and not provably dead", pid, state.Token, got)
		}
		if got := RunningPid(repo, name); got != 0 {
			t.Errorf("RunningPid(%s) = %d, want 0", name, got)
		}
		if got, held := StateOwner(repo, name); got != pid || !held {
			t.Errorf("StateOwner(%s) = (%d, %v), want (%d, true)", name, got, held, pid)
		}
		if !NeedsStop(repo, name) {
			t.Errorf("%s must keep the destructive-operation guard held", name)
		}
		if after, err := os.ReadFile(PidPath(repo, name)); err != nil || !bytes.Equal(after, before) {
			t.Errorf("%s record changed = (%q, %v), want %q", name, after, err, before)
		}
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
	if err := os.WriteFile(PidPath(repo, "other"), []byte(OwnerStateV1+"424242\n"+starttoken.Stale()+"\n"), 0o644); err != nil {
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
