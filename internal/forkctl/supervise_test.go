package forkctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestComposeTarget(t *testing.T) {
	cases := []struct {
		agent, model, effort, credential, want string
		wantErr                                bool
	}{
		{agent: "claude", want: "claude"},
		{agent: "claude", model: "opus-4.8", want: "claude:opus-4.8"},
		{agent: "claude", effort: "high", credential: "work", want: "claude/high@work"},
		{agent: "gemini", model: "gemini-3.5-flash@work", want: "gemini:gemini-3.5-flash@work"},
		{agent: "gemini", model: "gemini-3.5-flash@work", credential: "work", want: "gemini:gemini-3.5-flash@work"},
		{agent: "gemini", model: "gemini-3.5-flash@work", credential: "personal", wantErr: true},
	}
	for _, c := range cases {
		got, err := composeTarget(c.agent, c.model, c.effort, c.credential)
		if c.wantErr {
			if err == nil {
				t.Errorf("composeTarget(%q, %q, %q, %q) = %q, want error", c.agent, c.model, c.effort, c.credential, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("composeTarget(%q, %q, %q, %q) = (%q, %v), want %q", c.agent, c.model, c.effort, c.credential, got, err, c.want)
		}
	}
}

func TestForkStopMessages(t *testing.T) {
	repo := t.TempDir()
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	// A fork that doesn't exist → "no such fork" (matching ls/path/rm), not "not running".
	if code, err := a.ForkStop([]string{"ghost"}); code != 1 || err == nil || !strings.Contains(err.Error(), "no such fork") {
		t.Errorf("ForkStop(ghost) = (%d, %v), want (1, no such fork)", code, err)
	}
	// Stopping an already-idle fork is idempotent.
	if err := os.MkdirAll(forkspace.Workspace(repo, "idle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, err := a.ForkStop([]string{"idle"}); code != 0 || err != nil {
		t.Errorf("ForkStop(idle) = (%d, %v), want (0, nil)", code, err)
	}
}

// Nothing is written before the child exists, so a start that fails first leaves no record behind.
func TestDetachStartFailureLeavesNoRecord(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	name := "perf"
	if err := os.MkdirAll(forkspace.Workspace(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(forkspace.LogPath(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	if code, err := a.DetachForkLoop(repo, name, "codex", "", "", "", "", "", nil, nil); code != -1 || err == nil {
		t.Fatalf("detach with unwritable log target = (%d, %v), want startup failure", code, err)
	}
	if pathExists(forkspace.PidPath(repo, name)) {
		t.Fatal("failed detached start left a lifecycle record")
	}
}

func TestForkLogIsOwnerOnlyAndDoesNotFollowLinks(t *testing.T) {
	t.Run("new and reused regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fork.log")
		if err := os.WriteFile(path, []byte("old output\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		file, err := OpenForkLog(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("new output\n"); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("fork log mode = %v, err=%v; want 0600", info, err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "new output\n" {
			t.Fatalf("fork log did not preserve truncate-on-start behavior: %q, %v", got, err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside.log")
		if err := os.WriteFile(outside, []byte("keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "fork.log")
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		if file, err := OpenForkLog(path); err == nil {
			_ = file.Close()
			t.Fatal("symlinked fork log was accepted")
		}
		if got, err := os.ReadFile(outside); err != nil || string(got) != "keep\n" {
			t.Fatalf("symlink target changed: %q, %v", got, err)
		}
	})
}

// A child whose start died before its record still writes through that start's open of the log.
// Its refusal must land after a newer start's output, not over it.
func TestForkLogEarlierOpenAppendsAfterANewerStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fork.log")
	earlier, err := OpenForkLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer earlier.Close()
	newer, err := OpenForkLog(path)
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
	if got, err := os.ReadFile(path); err != nil || string(got) != "newer start output\nrefused\n" {
		t.Fatalf("log = %q, %v; want the refusal after the newer start's output", got, err)
	}
}

func TestForkStopRejectsInvalidStateWithoutSideEffects(t *testing.T) {
	stable := fmt.Sprintf("%d\n%s\n", os.Getpid(), forkspace.ProcStartToken(os.Getpid()))
	malformed := []string{"malformed", "left the exact file unchanged", "will not infer a process identity"}
	cases := []struct {
		name    string
		raw     string
		want    []string
		guarded bool // the refusal prints the manual recovery, in a safe order
	}{
		{name: "headerless worker", raw: stable, want: malformed, guarded: true},
		// Start and cleanup records of Coop 11.3.0 and earlier never become an identity to signal.
		// A cleanup record can name a worker that still runs, so the file goes only after a check.
		{name: "retired cleanup marker", raw: forkspace.OwnerStateV1 + "reap-pending\n" + stable, want: malformed, guarded: true},
		{name: "retired launch reservation", raw: forkspace.OwnerStateV1 + "start-launched\n" + stable, want: malformed, guarded: true},
		{name: "future owner", raw: "owner-v3\nopaque\n", want: []string{"unsupported detached-worker state version", "use the Coop version that wrote it", "do not edit its header"}},
	}
	oldSignal := forkspace.SignalPID
	signalCalls := 0
	forkspace.SignalPID = func(int, syscall.Signal) error {
		signalCalls++
		return nil
	}
	t.Cleanup(func() { forkspace.SignalPID = oldSignal })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			path := forkspace.PidPath(repo, "perf")
			if err := os.WriteFile(path, []byte(tc.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			runtimeCalls := 0
			a := &Control{
				cfg: &config.Config{RepoOverride: repo},
				host: Host{EnsureRuntime: func() (containerruntime.Runtime, error) {
					runtimeCalls++
					return containerruntime.Runtime{}, errors.New("runtime must not be consulted")
				}},
			}
			code, err := a.ForkStop([]string{"perf"})
			if code != 1 || err == nil {
				t.Fatalf("ForkStop unsupported state = (%d, %v), want (1, error)", code, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ForkStop error %q does not contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "restore") {
				t.Errorf("ForkStop error recommends fabricating state: %v", err)
			}
			if tc.guarded {
				steps := []string{
					"pgrep -fl -- '[f]ork perf .*--_detached'",
					"stop only one whose working directory is this repository",
					fmt.Sprintf("then remove the file: rm -- %q", path),
					"then run: coop fork stop perf",
				}
				last := -1
				for _, step := range steps {
					at := strings.Index(err.Error(), step)
					if at <= last {
						t.Fatalf("ForkStop error %q lacks %q after the previous recovery step", err, step)
					}
					last = at
				}
			}
			if runtimeCalls != 0 || signalCalls != 0 {
				t.Fatalf("unsupported stop performed side effects: runtime=%d signal=%d", runtimeCalls, signalCalls)
			}
			if got, readErr := os.ReadFile(path); readErr != nil || string(got) != tc.raw {
				t.Fatalf("unsupported state changed = %q, %v; want exact %q", got, readErr, tc.raw)
			}
		})
	}
}

// A Coop 11.3.0 cleanup record can name a worker that still runs. If someone removes the file
// without the check the refusal prints, the worker's own activity record still keeps stop from
// reaping its box and keeps a second loop controller from starting on the same fork.
func TestRemovedOldRecordStaysGuardedWhileItsWorkerRuns(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	workspace := forkspace.Workspace(repo, "perf")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "perf")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	// This test process stands in for the old worker that is still looping.
	worker, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{
		Kind: forkspace.ExecutionForkLoop, Role: forkspace.ExecutionRoleDetachedWorker,
		Workspace: workspace, Fork: &identity, SourceID: "old-worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, worker) })
	runtimeCalls := 0
	c := &Control{
		cfg: &config.Config{RepoOverride: repo},
		host: Host{EnsureRuntime: func() (containerruntime.Runtime, error) {
			runtimeCalls++
			return containerruntime.Runtime{}, errors.New("runtime must not be consulted")
		}},
	}
	if code, err := c.ForkStop([]string{"perf"}); code != 1 || err == nil || !strings.Contains(err.Error(), "still live or unverified") {
		t.Fatalf("stop without a record beside a live worker = (%d, %v), want a refusal", code, err)
	}
	if runtimeCalls != 0 {
		t.Fatal("stop reached the runtime while the worker still runs")
	}
	if _, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{
		Kind: forkspace.ExecutionForkLoop, Role: forkspace.ExecutionRoleDetachedWorker,
		Workspace: workspace, Fork: &identity, SourceID: "second-worker",
	}); err == nil || !strings.Contains(err.Error(), "already has running loop controller") {
		t.Fatalf("a second loop controller = %v, want a refusal", err)
	}
}

func TestForkStopFindsALeftoverNameWithoutWorkspace(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	runtimeCLI := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "stop", deadWorker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if names, err := forkspace.LifecycleNames(repo); err != nil || !slices.Contains(names, "stop") {
		t.Fatalf("lifecycle names = %v, want the leftover record's name", names)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}, rt: containerruntime.Runtime{Name: runtimeCLI}}
	if code, err := a.ForkStop([]string{"stop"}); code != 0 || err != nil {
		t.Fatalf("stop leftover fork = (%d, %v), want success", code, err)
	}
	if pathExists(forkspace.PidPath(repo, "stop")) {
		t.Fatal("successful stop kept the leftover record")
	}
}

func TestForkStopRejectsMalformedStateWithoutSignaling(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	worker := exec.Command("sleep", "30")
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = worker.Process.Kill()
		_ = worker.Wait()
	})
	// Bytes Coop cannot parse never become an identity to signal, even one naming a live process.
	if err := os.WriteFile(forkspace.PidPath(repo, "perf"), []byte(fmt.Sprintf("reap-pend\n%d\n", worker.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.ForkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "malformed") || !strings.Contains(err.Error(), "left the exact file unchanged") {
		t.Fatalf("ForkStop malformed state = (%d, %v), want actionable refusal", code, err)
	}
	if err := worker.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("malformed state must not signal a possibly live worker: %v", err)
	}
}

// nonRecords is what a box whose mount holds a fork's .coop folder could leave at a lifecycle
// record's name in place of a file Coop wrote, and how Coop names what it found.
var nonRecords = []struct {
	name  string
	found string
	plant func(path string) error
}{
	{name: "FIFO with no writer", found: "a named pipe", plant: func(path string) error { return syscall.Mkfifo(path, 0o600) }},
	{name: "link to dev zero", found: "a link", plant: func(path string) error { return os.Symlink("/dev/zero", path) }},
	{name: "dangling link", found: "a link", plant: func(path string) error { return os.Symlink(path+".gone", path) }},
	{name: "folder", found: "a folder", plant: func(path string) error { return os.Mkdir(path, 0o700) }},
	{name: "socket", found: "a socket", plant: plantSocket},
	{name: "oversized file", found: fmt.Sprintf("%d bytes, larger than any record", 1<<20), plant: func(path string) error {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return err
		}
		return os.Truncate(path, 1<<20) // sparse zeros, far past any record
	}},
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

// plantNonRecord leaves one of nonRecords at path and returns what it left, to show it unchanged.
func plantNonRecord(t *testing.T, path string, plant func(string) error) os.FileInfo {
	t.Helper()
	if err := plant(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func assertUnchanged(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() || after.Size() != before.Size() {
		t.Fatalf("%s changed: %v, %v", path, after, err)
	}
}

// finishesWithin runs fn and fails the test when it has not returned within wait.Deadline: a read
// that waits on a FIFO never returns. fn hands its results back through its closure.
func finishesWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(wait.Deadline):
		t.Fatalf("%s did not return within %v", what, wait.Deadline)
	}
}

// A fork's lifecycle record that Coop never wrote stops `coop fork stop` for that fork at once, with
// the guarded recovery and an inspection that cannot hang, and nothing is touched. `coop fork ls`
// still lists every fork, and stop still works for the others.
func TestForkStopAndLsRefuseStateThatIsNotARecord(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal")) // status runs git
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	oldSignal := forkspace.SignalPID
	t.Cleanup(func() { forkspace.SignalPID = oldSignal })
	for _, tc := range nonRecords {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			for _, name := range []string{"perf", "other"} {
				if err := os.MkdirAll(forkspace.Workspace(repo, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := forkspace.WriteWorkerState(repo, "other", deadWorker); err != nil {
				t.Fatal(err)
			}
			path := forkspace.PidPath(repo, "perf")
			before := plantNonRecord(t, path, tc.plant)
			signals := 0
			forkspace.SignalPID = func(pid int, sig syscall.Signal) error {
				signals++
				return oldSignal(pid, sig)
			}
			runtimeCalls := 0
			c := &Control{
				cfg: &config.Config{RepoOverride: repo},
				host: Host{EnsureRuntime: func() (containerruntime.Runtime, error) {
					runtimeCalls++
					return containerruntime.Runtime{}, errors.New("runtime must not be consulted")
				}},
			}
			var code int
			var err error
			finishesWithin(t, "coop fork stop perf", func() { code, err = c.ForkStop([]string{"perf"}) })
			if code != 1 || err == nil {
				t.Fatalf("stop over a %s = (%d, %v), want (1, error)", tc.name, code, err)
			}
			if !errors.Is(err, forkspace.ErrNotAStateFile) {
				t.Errorf("stop error %v does not match forkspace.ErrNotAStateFile", err)
			}
			remove := "rm"
			if tc.found == "a folder" {
				remove = "rmdir" // rm refuses a folder
			}
			steps := []string{
				fmt.Sprintf("fork perf state: %s is %s — Coop left it unchanged", path, tc.found),
				fmt.Sprintf("inspect it with: ls -ld %q", path),
				"pgrep -fl -- '[f]ork perf .*--_detached'",
				"stop only one whose working directory is this repository",
				fmt.Sprintf("then remove the file: %s -- %q", remove, path),
				"then run: coop fork stop perf",
			}
			last := -1
			for _, step := range steps {
				at := strings.Index(err.Error(), step)
				if at <= last {
					t.Fatalf("stop error %q lacks %q after the previous step", err, step)
				}
				last = at
			}
			if strings.Contains(err.Error(), "sed -n") {
				t.Errorf("stop error suggests reading a file that may never end: %v", err)
			}
			if runtimeCalls != 0 || signals != 0 {
				t.Fatalf("the refused stop had side effects: runtime=%d signals=%d", runtimeCalls, signals)
			}
			assertUnchanged(t, path, before)

			var notes []string
			ui.SetLiveSink(func(line string) { notes = append(notes, line) })
			t.Cleanup(func() { ui.SetLiveSink(nil) })
			out := captureStdout(t, func() {
				finishesWithin(t, "coop fork ls", func() { code, err = c.ForkLs(nil) })
			})
			if code != 0 || err != nil {
				t.Fatalf("ls beside a %s = (%d, %v), want success", tc.name, code, err)
			}
			for _, name := range []string{"perf", "other"} {
				if !strings.Contains(out, "\n  "+name+" · ") {
					t.Errorf("ls did not list fork %s:\n%s", name, out)
				}
			}
			if problem := fmt.Sprintf("fork perf worker: %s is %s", path, tc.found); !strings.Contains(strings.Join(notes, "\n"), problem) {
				t.Errorf("ls did not report %q:\n%s", problem, strings.Join(notes, "\n"))
			}

			runtimeCLI := filepath.Join(dir, "runtime")
			if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			other := &Control{cfg: &config.Config{RepoOverride: repo}, rt: containerruntime.Runtime{Name: runtimeCLI}}
			finishesWithin(t, "coop fork stop other", func() { code, err = other.ForkStop([]string{"other"}) })
			if code != 0 || err != nil {
				t.Fatalf("stop of the other fork = (%d, %v), want success", code, err)
			}
			if pathExists(forkspace.PidPath(repo, "other")) {
				t.Fatal("the other fork's stop kept its record")
			}
			assertUnchanged(t, path, before)
		})
	}
}

// Stop reads the record again under the lifecycle lock. One that a box swaps in after the unlocked
// preflight is refused there just as fast.
func TestForkStopRefusesStateThatIsNotARecordUnderTheLock(t *testing.T) {
	for _, tc := range nonRecords {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
				t.Fatal(err)
			}
			path := forkspace.PidPath(repo, "perf")
			var before os.FileInfo
			c := &Control{cfg: &config.Config{RepoOverride: repo}}
			got := runForkCommandAcrossLockedMutation(t, repo, "perf", func() (int, error) {
				return c.ForkStop([]string{"perf"})
			}, func() { before = plantNonRecord(t, path, tc.plant) })
			if got.code != 1 || !errors.Is(got.err, forkspace.ErrNotAStateFile) || !strings.Contains(got.err.Error(), path+" is "+tc.found) {
				t.Fatalf("stop across a planted %s = (%d, %v), want the refusal", tc.name, got.code, got.err)
			}
			assertUnchanged(t, path, before)
		})
	}
}

func TestForkStopRejectsPIDOneWithoutSignaling(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkspace.PidPath(repo, "perf"), []byte(forkspace.OwnerStateV1+"1\nlinux-proc-v1:fake:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSignal := forkspace.SignalPID
	called := false
	forkspace.SignalPID = func(int, syscall.Signal) error {
		called = true
		return nil
	}
	t.Cleanup(func() { forkspace.SignalPID = oldSignal })
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.ForkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("ForkStop(pid 1) = (%d, %v), want malformed-state refusal", code, err)
	}
	if called {
		t.Fatal("pid 1 state must be rejected before any liveness probe or signal")
	}
}

func TestForkStopReportsCleanupStateRemovalFailure(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	runtimeCLI := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "perf", deadWorker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
if [ "$1" = ps ]; then
	rm -f "$COOP_TEST_PID_PATH"
	mkdir "$COOP_TEST_PID_PATH"
	: > "$COOP_TEST_PID_PATH/blocker"
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_PID_PATH", forkspace.PidPath(repo, "perf"))
	a := &Control{
		cfg: &config.Config{RepoOverride: repo},
		rt:  containerruntime.Runtime{Name: runtimeCLI},
	}
	code, err := a.ForkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "cleanup state could not be cleared") || !strings.Contains(err.Error(), "coop fork stop perf") {
		t.Fatalf("ForkStop state removal failure = (%d, %v), want actionable error", code, err)
	}
}

func TestForkStopReapsBoxAfterWorkerCrash(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	runtimeCLI := filepath.Join(dir, "runtime")
	events := filepath.Join(dir, "events")
	if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "perf", deadWorker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
if [ "$1" = ps ]; then printf '%s\n' box-perf; fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", events)
	a := &Control{
		cfg: &config.Config{RepoOverride: repo},
		rt:  containerruntime.Runtime{Name: runtimeCLI},
	}
	if code, err := a.ForkStop([]string{"perf"}); code != 0 || err != nil {
		t.Fatalf("ForkStop after worker crash = (%d, %v), want success", code, err)
	}
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	want := "ps -q -a --filter label=coop.fork-owner=" + ForkContainerOwner(repo, "perf") + "\nrm -f box-perf\n"
	if got := string(data); got != want {
		t.Errorf("crash cleanup calls = %q, want %q", got, want)
	}
	if pathExists(forkspace.PidPath(repo, "perf")) {
		t.Fatal("successful crash cleanup should remove worker state")
	}
}

func TestForkStopReapsPidlessDetachedWorkerWithoutTouchingForegroundActivity(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	workspace := forkspace.Workspace(repo, "perf")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "perf")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	foreground, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{
		Kind: forkspace.ExecutionForkInteractive, Role: forkspace.ExecutionRoleSandbox,
		Workspace: workspace, Fork: &identity, SourceID: "foreground",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, foreground) })
	stale := forkspace.ExecutionRecord{
		Version: 1, ID: strings.Repeat("a", 32), Kind: forkspace.ExecutionForkLoop,
		Role: forkspace.ExecutionRoleDetachedWorker, Workspace: workspace, Fork: &identity,
		SourceID: "dead-worker", PID: 2147483646, Token: "linux-proc-v1:1:2", StartedAt: time.Now().UTC(),
	}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	executionDir := filepath.Join(forkspace.StateDir(repo), "executions")
	if err := os.MkdirAll(executionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(executionDir, stale.ID+".json"), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeCLI := filepath.Join(dir, "runtime")
	events := filepath.Join(dir, "events")
	if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
if [ "$1" = ps ]; then printf '%s\n' detached-box; fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", events)
	c := &Control{cfg: &config.Config{RepoOverride: repo}, rt: containerruntime.Runtime{Name: runtimeCLI}}
	if code, err := c.ForkStop([]string{"perf"}); code != 0 || err != nil {
		t.Fatalf("pidless detached cleanup = (%d, %v)", code, err)
	}
	data, err := os.ReadFile(events)
	if err != nil || !strings.Contains(string(data), "label=coop.fork-worker="+box.LabelOn) {
		t.Fatalf("runtime cleanup was not worker-scoped: %q, err=%v", data, err)
	}
	observations, problems := forkspace.Executions(repo)
	if len(problems) != 0 || len(observations) != 1 || observations[0].Record.ID != foreground.ID {
		t.Fatalf("stop changed foreground activity: %+v, problems=%v", observations, problems)
	}
}

func TestForkStopRetiresProvablyDeadForegroundExecution(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	workspace := forkspace.Workspace(repo, "stale-foreground")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "stale-foreground")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "stale-foreground")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	stale := forkspace.ExecutionRecord{
		Version: 1, ID: strings.Repeat("b", 32), Kind: forkspace.ExecutionForkInteractive,
		Role: forkspace.ExecutionRoleSandbox, Workspace: workspace, Fork: &identity,
		SourceID: "dead-foreground", PID: 2147483646, Token: "linux-proc-v1:1:2", StartedAt: time.Now().UTC(),
	}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	executionDir := filepath.Join(forkspace.StateDir(repo), "executions")
	if err := os.MkdirAll(executionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(executionDir, stale.ID+".json"), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeCLI := filepath.Join(dir, "runtime")
	events := filepath.Join(dir, "events")
	if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
if [ "$1" = ps ]; then printf '%s\n' stale-box; fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", events)
	c := &Control{cfg: &config.Config{RepoOverride: repo}, rt: containerruntime.Runtime{Name: runtimeCLI}}
	if code, err := c.ForkStop([]string{"stale-foreground"}); code != 0 || err != nil {
		t.Fatalf("stale foreground cleanup = (%d, %v)", code, err)
	}
	data, err := os.ReadFile(events)
	if err != nil || !strings.Contains(string(data), "label="+box.LabelExecution+"="+stale.ID) ||
		strings.Contains(string(data), "label="+box.LabelForkWorker+"=") {
		t.Fatalf("stale foreground cleanup labels = %q, err=%v", data, err)
	}
	unlock, err = forkspace.LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := forkspace.RequireNoForkExecutionsLocked(repo, identity); err != nil {
		t.Fatalf("stale foreground still blocks lifecycle: %v", err)
	}
}

func TestForkStopRetiresStaleLoopSandboxBeforeSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, container, failure     string
		keep, live, unknown, foreign bool
		kind                         forkspace.ExecutionKind
	}{
		{name: "orphan present", container: "sandbox-box"},
		{name: "already absent"},
		{name: "query failure", failure: "ps"},
		{name: "remove failure", container: "sandbox-box", failure: "rm"},
		{name: "registry error", failure: "inventory"},
		{name: "live sandbox", live: true, keep: true},
		{name: "unknown owner", unknown: true, keep: true},
		{name: "other generation", foreign: true, keep: true},
		{name: "foreground", kind: forkspace.ExecutionForkInteractive, keep: true},
		{name: "editor", kind: forkspace.ExecutionForkACP, keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			workspace := forkspace.Workspace(repo, "perf")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			unlock, err := forkspace.LockState(repo, "perf")
			if err != nil {
				t.Fatal(err)
			}
			identity, err := forkspace.EnsureGenerationLocked(repo, "perf")
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := forkspace.WriteWorkerState(repo, "perf", forkspace.WorkerState{Pid: deadWorker.Pid, Token: deadWorker.Token, Generation: identity.Generation}); err != nil {
				t.Fatal(err)
			}
			stale := forkspace.ExecutionRecord{Version: 1, ID: strings.Repeat("c", 32), Kind: forkspace.ExecutionForkLoop, Role: forkspace.ExecutionRoleSandbox,
				Workspace: workspace, Fork: &identity, PID: 2147483646, Token: "linux-proc-v1:1:2", StartedAt: time.Now().UTC()}
			if tc.kind != "" {
				stale.Kind = tc.kind
			}
			if tc.live {
				stale.PID, stale.Token = os.Getpid(), forkspace.ProcStartToken(os.Getpid())
			}
			if tc.unknown {
				oldSignal := forkspace.SignalPID
				forkspace.SignalPID = func(pid int, sig syscall.Signal) error {
					if pid == stale.PID && sig == 0 {
						return syscall.EPERM
					}
					return oldSignal(pid, sig)
				}
				t.Cleanup(func() { forkspace.SignalPID = oldSignal })
			}
			if tc.foreign {
				other := identity
				other.Generation = forkspace.Generation(strings.Repeat("d", 32))
				stale.Fork = &other
			}
			executionDir := filepath.Join(forkspace.StateDir(repo), "executions")
			if err := os.MkdirAll(executionDir, 0o700); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(stale)
			if err != nil {
				t.Fatal(err)
			}
			recordPath := filepath.Join(executionDir, stale.ID+".json")
			if err := os.WriteFile(recordPath, append(body, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.failure == "inventory" {
				if err := os.WriteFile(filepath.Join(executionDir, "broken.json"), []byte("invalid\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			runtimeCLI, events := filepath.Join(dir, "runtime"), filepath.Join(dir, "events")
			if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
case "$*" in
  *coop.execution=*)
    if [ "$COOP_TEST_RUNTIME_FAILURE" = ps ]; then exit 42; fi
    if [ -n "$COOP_TEST_CONTAINER_ID" ]; then printf '%s\n' "$COOP_TEST_CONTAINER_ID"; fi ;;
esac
if [ "$1" = rm ] && [ "$COOP_TEST_RUNTIME_FAILURE" = rm ]; then exit 42; fi
`), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("COOP_TEST_EVENTS", events)
			t.Setenv("COOP_TEST_CONTAINER_ID", tc.container)
			t.Setenv("COOP_TEST_RUNTIME_FAILURE", tc.failure)
			c := &Control{cfg: &config.Config{RepoOverride: repo}, rt: containerruntime.Runtime{Name: runtimeCLI}}
			code, stopErr := c.ForkStop([]string{"perf"})
			if tc.failure != "" {
				if code != 1 || stopErr == nil || !strings.Contains(stopErr.Error(), "coop fork stop perf") || !pathExists(recordPath) || !pathExists(forkspace.PidPath(repo, "perf")) {
					t.Fatalf("failed exact sandbox cleanup lost retry authority: code=%d err=%v", code, stopErr)
				}
				if tc.failure == "inventory" {
					if err := os.Remove(filepath.Join(executionDir, "broken.json")); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("COOP_TEST_RUNTIME_FAILURE", "")
				code, stopErr = c.ForkStop([]string{"perf"})
			}
			if code != 0 || stopErr != nil || pathExists(forkspace.PidPath(repo, "perf")) {
				t.Fatalf("stop=%d err=%v", code, stopErr)
			}
			if pathExists(recordPath) != tc.keep {
				t.Fatalf("first stop retained=%v, want %v", pathExists(recordPath), tc.keep)
			}
			calls, err := os.ReadFile(events)
			if err != nil {
				t.Fatal(err)
			}
			if exact := strings.Contains(string(calls), "label="+box.LabelExecution+"="+stale.ID); exact == tc.keep {
				t.Fatalf("wrong cleanup scope: %s", calls)
			}
			if !tc.keep {
				for _, call := range strings.Split(string(calls), "\n") {
					if !strings.Contains(call, "label="+box.LabelExecution+"="+stale.ID) {
						continue
					}
					if !strings.HasPrefix(call, "ps ") || !strings.Contains(call, "label="+box.LabelForkOwner+"="+ForkContainerOwner(repo, "perf", identity.Generation)) || !strings.Contains(call, "label="+box.LabelForkGeneration+"="+string(identity.Generation)) {
						t.Fatalf("exact sandbox query missing generation scope: %s", call)
					}
				}
				if code, err := c.ForkStop([]string{"perf"}); code != 0 || err != nil {
					t.Fatalf("idempotent stop=%d err=%v", code, err)
				}
			}
		})
	}
}

// ForkStop signals the detached worker before reaping only that fork's labeled box. The runtime
// shim makes both the orphan-present and already-gone paths deterministic without a real daemon.
func TestForkStopReapsBoxAfterWorkerExit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		containerID string
		failure     string
	}{
		{name: "orphan present", containerID: "box-perf"},
		{name: "already gone"},
		{name: "query failure is retryable", containerID: "box-perf", failure: "ps"},
		{name: "remove failure is not success", containerID: "box-perf", failure: "rm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			events := filepath.Join(dir, "events")
			runtimeCLI := filepath.Join(dir, "runtime")
			if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
printf 'runtime:%s\n' "$*" >> "$COOP_TEST_EVENTS"
if kill -0 "$COOP_TEST_WORKER_PID" 2>/dev/null; then
	printf 'runtime:worker-still-alive\n' >> "$COOP_TEST_EVENTS"
fi
if [ "$COOP_TEST_RUNTIME_FAILURE" = "$1" ]; then
	exit 42
fi
if [ "$1" = ps ] && [ -n "$COOP_TEST_CONTAINER_ID" ]; then
	printf '%s\n' "$COOP_TEST_CONTAINER_ID"
fi
`), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("COOP_TEST_EVENTS", events)
			t.Setenv("COOP_TEST_CONTAINER_ID", tc.containerID)
			t.Setenv("COOP_TEST_RUNTIME_FAILURE", tc.failure)

			// The worker acknowledges TERM through a trap. The sleep runs in the BACKGROUND with the
			// shell in `wait`, never as a foreground command: a POSIX shell defers a trap until its
			// foreground child completes, so with `sleep 10` in the foreground the acknowledgement
			// depended on the child dying from the same group signal. Any schedule where the child
			// survives it — the bare-pid fallback, a signal landing between fork and exec — deferred
			// the trap for the full ten seconds, past production's three-second grace, and the KILL
			// that followed erased the acknowledgement the test asserts. `wait` is interruptible by a
			// trap, so this shape acknowledges within milliseconds whatever the child does.
			worker := exec.Command("sh", "-c", `
trap 'printf "worker:term\n" >> "$COOP_TEST_EVENTS"; exit 0' TERM
printf "worker:ready\n" >> "$COOP_TEST_EVENTS"
while :; do sleep 10 & wait $!; done
`)
			worker.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := worker.Start(); err != nil {
				t.Fatal(err)
			}
			pid := worker.Process.Pid
			workerDone := make(chan struct{})
			go func() {
				// Record how the worker actually ended, so a failure shows signal versus exit
				// evidence instead of leaving the schedule to be guessed at from timestamps.
				err := worker.Wait()
				state := "exit status 0"
				if worker.ProcessState != nil {
					state = worker.ProcessState.String()
				}
				if err != nil && worker.ProcessState == nil {
					state = err.Error()
				}
				f, _ := os.OpenFile(events, os.O_APPEND|os.O_WRONLY, 0o644)
				if f != nil {
					fmt.Fprintf(f, "worker:exit %s\n", state)
					f.Close()
				}
				close(workerDone)
			}()
			t.Setenv("COOP_TEST_WORKER_PID", strconv.Itoa(pid))
			t.Cleanup(func() {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				select {
				case <-workerDone:
				case <-time.After(2 * time.Second):
					t.Errorf("worker process group %d did not exit", pid)
				}
			})

			deadline := time.Now().Add(wait.Deadline) // a fixture guard, not the behavior under test
			for {
				data, _ := os.ReadFile(events)
				if strings.Contains(string(data), "worker:ready\n") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker did not become ready")
				}
				time.Sleep(10 * time.Millisecond)
			}

			if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			token := forkspace.ProcStartToken(pid)
			if token == "" {
				t.Fatal("could not read worker start identity")
			}
			if err := forkspace.WriteWorkerState(repo, "perf", forkspace.WorkerState{Pid: pid, Token: token}); err != nil {
				t.Fatal(err)
			}
			a := &Control{
				cfg: &config.Config{RepoOverride: repo},
				rt:  containerruntime.Runtime{Name: runtimeCLI},
			}

			code, stopErr := a.ForkStop([]string{"perf"})
			if tc.failure == "" {
				if code != 0 || stopErr != nil {
					t.Fatalf("ForkStop(perf) = (%d, %v), want (0, nil)", code, stopErr)
				}
			} else if code != 1 || stopErr == nil || !strings.Contains(stopErr.Error(), "box reap failed") || !strings.Contains(stopErr.Error(), "coop fork stop perf") {
				t.Fatalf("ForkStop(perf) = (%d, %v), want (1, box reap failed)", code, stopErr)
			}
			data, err := os.ReadFile(events)
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			termAt := strings.Index(got, "worker:term\n")
			psCall := "runtime:ps -q -a --filter label=coop.fork-owner=" + ForkContainerOwner(repo, "perf") + "\n"
			psAt := strings.Index(got, psCall)
			if termAt < 0 || psAt < 0 || termAt >= psAt {
				t.Errorf("worker TERM must precede the exact-fork reap:\n%s", got)
			}
			// The trap's `exit 0` is the acknowledgement; a worker reaped by SIGKILL never sent it,
			// and this line says which happened rather than leaving it to the timing above.
			if !strings.Contains(got, "worker:exit exit status 0\n") {
				t.Errorf("worker did not exit through its TERM trap:\n%s", got)
			}
			if strings.Contains(got, "runtime:worker-still-alive\n") {
				t.Errorf("runtime reap started before the worker exited:\n%s", got)
			}
			rmCall := "runtime:rm -f " + tc.containerID + "\n"
			wantRuntime := psCall
			if tc.containerID != "" && tc.failure != "ps" {
				wantRuntime += rmCall
			}
			if tc.failure != "" {
				// The failed stop left the dead worker's record unchanged: it is the retry handle, and a
				// new start refuses it until the box cleanup has run.
				if state, err := forkspace.ReadWorkerState(repo, "perf"); err != nil || state.Pid != pid || state.Token != token {
					t.Fatalf("record after a failed reap = (%+v, %v), want the worker pid %d", state, err, pid)
				}
				if err := requireNoWorkerLocked(repo, "perf"); err == nil || !strings.Contains(err.Error(), "coop fork stop perf") {
					t.Fatalf("start during pending cleanup = %v, want actionable refusal", err)
				}
				t.Setenv("COOP_TEST_RUNTIME_FAILURE", "")
				if retryCode, retryErr := a.ForkStop([]string{"perf"}); retryCode != 0 || retryErr != nil {
					t.Fatalf("ForkStop(perf) retry = (%d, %v), want (0, nil)", retryCode, retryErr)
				}
				wantRuntime += psCall
				if tc.containerID != "" {
					wantRuntime += rmCall
				}
				data, err = os.ReadFile(events)
				if err != nil {
					t.Fatal(err)
				}
				got = string(data)
			}
			var gotRuntime strings.Builder
			for _, line := range strings.SplitAfter(got, "\n") {
				if strings.HasPrefix(line, "runtime:") {
					gotRuntime.WriteString(line)
				}
			}
			if gotRuntime.String() != wantRuntime {
				t.Errorf("runtime calls = %q, want exact-fork reap %q", gotRuntime.String(), wantRuntime)
			}
			if pathExists(forkspace.PidPath(repo, "perf")) {
				t.Error("pidfile should be removed after a successful reap or retry")
			}
		})
	}
}

// deadWorker names a pid the kernel answers ESRCH for, with a stable token shape: the record a
// crashed worker leaves behind. Its pid differs from the execution-record fixtures' 2147483646,
// which some tests make unverifiable.
var deadWorker = forkspace.WorkerState{Pid: 2147483645, Token: "linux-proc-v1:1:2"}

// runningForkNames is the destructive-operation guard: a live loop counts, as does a dead worker's
// record that may still own an orphaned box; only an absent one does not.
func TestRunningForkNames(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := forkspace.WritePid(repo, "live", os.Getpid(), ""); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "dead", deadWorker); err != nil {
		t.Fatal(err)
	}
	got := runningForkNames(repo, []string{"live", "dead", "absent"})
	if !reflect.DeepEqual(got, []string{"live", "dead"}) {
		t.Errorf("runningForkNames = %v, want [live dead]", got)
	}
}

// detachFixture is a fork workspace whose log path is a directory, so a start that wrongly passed
// its lifecycle check fails at the log instead of re-executing the test binary.
func detachFixture(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.LogPath(repo, "perf"), 0o700); err != nil {
		t.Fatal(err)
	}
	return repo
}

// A detached start refuses while the fork has any lifecycle record and leaves that record exactly
// as it was: overwriting a live worker's is the orphan bug, and a dead worker's record is the handle
// `coop fork stop` needs to reap the box that worker may have left.
func TestDetachForkLoopRefusesAnExistingRecord(t *testing.T) {
	for _, tc := range []struct {
		name         string
		write        func(repo string) error
		unverifiable bool // the recorded pid answers signals, but its start token can no longer be read
		want         string
	}{
		{name: "live worker", write: func(repo string) error {
			return forkspace.WritePid(repo, "perf", os.Getpid(), "")
		}, want: fmt.Sprintf("already has a loop running (pid %d) — stop it first: coop fork stop perf", os.Getpid())},
		{name: "leftover record", write: func(repo string) error {
			return forkspace.WriteWorkerState(repo, "perf", deadWorker)
		}, want: "has a leftover record from an earlier background loop — clean it up first: coop fork stop perf"},
		{name: "unverifiable record", write: func(repo string) error {
			return forkspace.WritePid(repo, "perf", os.Getpid(), "")
		}, unverifiable: true, want: "still has a background loop record — clean it up first: coop fork stop perf"},
		{name: "malformed record", write: func(repo string) error {
			return os.WriteFile(forkspace.PidPath(repo, "perf"), []byte("garbage\n"), 0o600)
		}, want: "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := detachFixture(t)
			if err := tc.write(repo); err != nil {
				t.Fatal(err)
			}
			if tc.unverifiable {
				oldRead := forkspace.ReadProcStartToken
				forkspace.ReadProcStartToken = func(int) string { return "" }
				t.Cleanup(func() { forkspace.ReadProcStartToken = oldRead })
			}
			before, err := os.ReadFile(forkspace.PidPath(repo, "perf"))
			if err != nil {
				t.Fatal(err)
			}
			a := &Control{cfg: &config.Config{}}
			code, err := a.DetachForkLoop(repo, "perf", "claude", "", "", "", "", "", nil, nil)
			if code != 1 || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DetachForkLoop over a %s = (%d, %v), want (1, %q)", tc.name, code, err, tc.want)
			}
			if after, err := os.ReadFile(forkspace.PidPath(repo, "perf")); err != nil || !bytes.Equal(after, before) {
				t.Fatalf("the refused start changed the record = (%q, %v), want %q", after, err, before)
			}
		})
	}
}

// A detached start refuses a record Coop never wrote at once, with the guarded recovery, and leaves
// it as it was.
func TestDetachForkLoopRefusesStateThatIsNotARecord(t *testing.T) {
	for _, tc := range nonRecords {
		t.Run(tc.name, func(t *testing.T) {
			repo := detachFixture(t)
			path := forkspace.PidPath(repo, "perf")
			before := plantNonRecord(t, path, tc.plant)
			a := &Control{cfg: &config.Config{}}
			var code int
			var err error
			finishesWithin(t, "a detached start", func() {
				code, err = a.DetachForkLoop(repo, "perf", "claude", "", "", "", "", "", nil, nil)
			})
			if code != 1 || !errors.Is(err, forkspace.ErrNotAStateFile) || !strings.Contains(err.Error(), path+" is "+tc.found) ||
				!strings.Contains(err.Error(), "then run: coop fork stop perf") {
				t.Fatalf("start over a %s = (%d, %v), want the refusal and its recovery", tc.name, code, err)
			}
			assertUnchanged(t, path, before)
		})
	}
}

// The start checks for a record under the lifecycle lock: a worker recorded while the start waited
// for that lock is refused, not overwritten.
func TestDetachForkLoopChecksUnderTheLifecycleLock(t *testing.T) {
	repo := detachFixture(t)
	a := &Control{cfg: &config.Config{}}
	got := runForkCommandAcrossLockedMutation(t, repo, "perf", func() (int, error) {
		return a.DetachForkLoop(repo, "perf", "claude", "", "", "", "", "", nil, nil)
	}, func() {
		if err := forkspace.WritePid(repo, "perf", os.Getpid(), ""); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "already has a loop running") {
		t.Fatalf("start across a newly recorded worker = (%d, %v), want a refusal", got.code, got.err)
	}
	if forkspace.RunningPid(repo, "perf") != os.Getpid() {
		t.Fatal("the start overwrote the worker recorded while it waited")
	}
}

// A child whose record cannot be written is killed and reaped before the start reports failure, and
// no record is left: no loop may run, even briefly, without a stop handle.
func TestRecordStartedForkKillsChildWhenStateWriteFails(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	// An invalid generation makes the write fail deterministically, before anything reaches the disk.
	if err := recordStartedFork(repo, "perf", cmd, forkspace.Generation("not a generation")); err == nil {
		t.Fatal("recordStartedFork should report the failed write")
	}
	if cmd.ProcessState == nil {
		t.Fatal("child must be killed and reaped when its record cannot be written")
	}
	if pathExists(forkspace.PidPath(repo, "perf")) {
		t.Fatal("a failed write left a lifecycle record")
	}
}

// waitForExit returns immediately-true for a dead pid and false (after the timeout) for a live one
// — ForkStop uses it to confirm death before clearing the pidfile.
func TestWaitForExit(t *testing.T) {
	if exited, err := waitForExit(2147483646, "", 2*time.Second); !exited || err != nil { // a pid that isn't running
		t.Errorf("waitForExit(dead) = (%v, %v), want (true, nil)", exited, err)
	}
	token := forkspace.ProcStartToken(os.Getpid())
	if exited, err := waitForExit(os.Getpid(), token, 80*time.Millisecond); exited || err != nil { // we're alive
		t.Errorf("waitForExit(live) = (%v, %v), want (false, nil)", exited, err)
	}
}

func TestStreamLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.log")
	if err := os.WriteFile(p, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var buf bytes.Buffer
	if opened, err := streamLog(p, "", false, &buf, &mu); err != nil || !opened {
		t.Fatal(err)
	}
	if buf.String() != "line1\nline2\n" {
		t.Errorf("streamLog = %q, want unprefixed lines", buf.String())
	}
	buf.Reset()
	_, _ = streamLog(p, "perf", false, &buf, &mu)
	if buf.String() != "perf | line1\nperf | line2\n" {
		t.Errorf("streamLog prefixed = %q", buf.String())
	}
	// A missing log is not an error and produces nothing.
	buf.Reset()
	if opened, err := streamLog(filepath.Join(dir, "missing.log"), "", false, &buf, &mu); err != nil || opened {
		t.Fatalf("missing log = (opened %v, %v), want absent success", opened, err)
	}
	if buf.Len() != 0 {
		t.Errorf("missing log produced %q", buf.String())
	}
	if opened, err := streamLog(dir, "", false, &buf, &mu); err == nil || !opened {
		t.Fatalf("directory log = (opened %v, %v), want read error", opened, err)
	}
	// A box can leave a FIFO no one writes or a link to an endless device at a log's name. Either
	// is refused at once, followed or not, and nothing is printed.
	fifo, link := filepath.Join(dir, "fifo.log"), filepath.Join(dir, "link.log")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", link); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	for path, found := range map[string]string{fifo: "a named pipe", link: "a link"} {
		for _, follow := range []bool{false, true} {
			var err error
			finishesWithin(t, fmt.Sprintf("streamLog(%s, follow=%v)", path, follow), func() {
				_, err = streamLog(path, "", follow, &buf, &mu)
			})
			if !errors.Is(err, forkspace.ErrNotAStateFile) || err.Error() != path+" is "+found {
				t.Errorf("streamLog(%s, follow=%v) = %v, want the refusal naming %s", path, follow, err, found)
			}
		}
	}
	if buf.Len() != 0 {
		t.Errorf("refused logs printed %q", buf.String())
	}
}

// largestWrite records output and the largest single write streamLog made.
type largestWrite struct {
	bytes.Buffer
	largest int
}

func (w *largestWrite) Write(p []byte) (int, error) {
	w.largest = max(w.largest, len(p))
	return w.Buffer.Write(p)
}

// A log line that never ends prints in pieces, so streamLog never holds more than logChunkLimit of
// it. A log as such has no limit: every byte still prints, unchanged.
func TestStreamLogHoldsAtMostOneChunkOfALine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	content := append(bytes.Repeat([]byte("x"), 3*logChunkLimit+5), "\nend\n"...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	w := &largestWrite{}
	if opened, err := streamLog(path, "", false, w, &mu); err != nil || !opened {
		t.Fatalf("streamLog = (opened %v, %v), want success", opened, err)
	}
	if !bytes.Equal(w.Bytes(), content) {
		t.Fatalf("streamLog printed %d bytes, want the log's %d unchanged", w.Len(), len(content))
	}
	if w.largest > logChunkLimit {
		t.Fatalf("streamLog wrote %d bytes at once, want at most %d", w.largest, logChunkLimit)
	}
}

// In the view of every fork, each piece of a line too long to hold, and a last line with no newline
// yet, prints as a line of its own, so the next fork's prefix never lands mid-line.
func TestStreamLogEndsEveryPrefixedPiece(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	long := strings.Repeat("x", 2*logChunkLimit+5)
	if err := os.WriteFile(path, []byte(long+"\nunfinished"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var buf bytes.Buffer
	if opened, err := streamLog(path, "perf", false, &buf, &mu); err != nil || !opened {
		t.Fatalf("streamLog = (opened %v, %v), want success", opened, err)
	}
	want := "perf | " + long[:logChunkLimit] + "\n" +
		"perf | " + long[logChunkLimit:2*logChunkLimit] + "\n" +
		"perf | " + long[2*logChunkLimit:] + "\n" +
		"perf | unfinished\n"
	if got := buf.String(); got != want {
		t.Fatalf("streamLog printed %d bytes in %d lines, want %d bytes in %d prefixed lines",
			len(got), strings.Count(got, "\n"), len(want), strings.Count(want, "\n"))
	}
}

func TestForkLogsNamedMissingIsExplicit(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(forkspace.Workspace(repo, "quiet"), 0o755); err != nil {
		t.Fatal(err)
	}
	var notes []string
	ui.SetLiveSink(func(line string) { notes = append(notes, line) })
	t.Cleanup(func() { ui.SetLiveSink(nil) })
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	if code, err := c.ForkLogs([]string{"quiet"}); code != 0 || err != nil {
		t.Fatalf("ForkLogs(quiet) = (%d, %v), want empty success", code, err)
	}
	if got := strings.Join(notes, "\n"); !strings.Contains(got, "Fork quiet has no log output yet.") {
		t.Fatalf("missing-log note = %q", got)
	}
}

func TestForkLogsAllKeepsReadableOutputAndReturnsFailures(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	for _, name := range []string{"good", "broken"} {
		if err := os.MkdirAll(forkspace.Workspace(repo, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkspace.LogPath(repo, "good"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(forkspace.LogPath(repo, "broken"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	var code int
	var runErr error
	out := captureStdout(t, func() { code, runErr = c.ForkLogs(nil) })
	if code != 1 || runErr == nil || !strings.Contains(runErr.Error(), "broken") ||
		!strings.Contains(runErr.Error(), forkspace.LogPath(repo, "broken")) {
		t.Fatalf("ForkLogs(all) = (%d, %v), want broken-log failure", code, runErr)
	}
	if !strings.Contains(out, "good | kept\n") {
		t.Fatalf("readable log output was lost: %q", out)
	}
}

// A FIFO or a link at one fork's log fails that fork at once with how to remove it, since no
// permission change makes it a log; every other fork's log still prints.
func TestForkLogsAllSurvivesALogThatIsNotAFile(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	for _, name := range []string{"fifo", "good", "link"} {
		if err := os.MkdirAll(forkspace.Workspace(repo, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkspace.LogPath(repo, "good"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(forkspace.LogPath(repo, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", forkspace.LogPath(repo, "link")); err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	var code int
	var runErr error
	out := captureStdout(t, func() {
		finishesWithin(t, "coop fork logs", func() { code, runErr = c.ForkLogs(nil) })
	})
	if code != 1 || runErr == nil {
		t.Fatalf("ForkLogs(all) = (%d, %v), want both refused logs named", code, runErr)
	}
	for name, found := range map[string]string{"fifo": "a named pipe", "link": "a link"} {
		path := forkspace.LogPath(repo, name)
		if want := fmt.Sprintf("fork %s log: %s is %s — remove it: rm -- %q", name, path, found, path); !strings.Contains(runErr.Error(), want) {
			t.Errorf("ForkLogs(all) error lacks %q:\n%v", want, runErr)
		}
	}
	if strings.Contains(runErr.Error(), "permissions") {
		t.Errorf("ForkLogs(all) suggests a permission change for what is not a log: %v", runErr)
	}
	if out != "good | kept\n" {
		t.Fatalf("ForkLogs(all) printed %q, want only the readable log", out)
	}
}

func TestForkLogsFollowReportsStreamFailure(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(forkspace.Workspace(repo, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(forkspace.LogPath(repo, "broken"), 0o700); err != nil {
		t.Fatal(err)
	}
	var lines []string
	ui.SetLiveSink(func(line string) { lines = append(lines, line) })
	t.Cleanup(func() { ui.SetLiveSink(nil) })
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	code, err := c.ForkLogs([]string{"--follow"})
	if code != 1 || err == nil {
		t.Fatalf("ForkLogs(--follow) = (%d, %v), want stream failure", code, err)
	}
	path := forkspace.LogPath(repo, "broken")
	if got, want := strings.Join(lines, "\n"), fmt.Sprintf("fork broken log: %s is a folder — remove it: rmdir -- %q", path, path); !strings.Contains(got, want) {
		t.Fatalf("follow failure output = %q, want %q", got, want)
	}
}
