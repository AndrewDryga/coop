package consult

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// straggler is what the peer leaves holding the pipe: long enough that it cannot end on its own
// inside the test, and distinctive enough to recognize in the process table.
const straggler = "sleep 604"

// A capture is a shell wrapped around the reader that is blocked on the peer's FIFO. Signalling
// the shell alone left that reader alive — it survives the signal its owner received, is
// reparented out of the tree, and stays in the process group the box was launched in. That is how
// a scripted provider run ended with a process nobody owned still in coop's group, and it is the
// shape this test pins: after the wrapper is gone, no reader it started may remain.
//
// The process table is the evidence, deliberately. A sleep proves the reader was slow, not that it
// was owned.
func TestConsultWrapperStoppedCaptureLeavesNoReader(t *testing.T) {
	for _, streamLimit := range []struct{ name, value string }{
		{"unlimited drain (cat)", ""},
		{"bounded drain (dd)", "65536"},
	} {
		t.Run(streamLimit.name, func(t *testing.T) {
			dir := consultStubDir(t)
			// The peer leaves a background process holding its stdout — the capture's FIFO — and
			// exits. That is the one shape where a reader is genuinely stuck: the wrapper waits for
			// the peer, sees it gone, and the pipe still has a writer.
			writeStub(t, dir, "gemini", "printf 'partial reply'\n"+straggler+" &\nexit 0")

			cmd := consultWrapperCommand(t, dir, streamLimit.value)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			group := cmd.Process.Pid
			defer func() { _ = syscall.Kill(-group, syscall.SIGKILL) }()

			// Wait for the state that actually matters, not merely for a reader to exist: the peer
			// gone and its straggler holding the pipe. Readers appear the instant a capture starts,
			// long before the peer has written anything.
			deadline := time.Now().Add(wait.Deadline)
			for {
				snapshot := groupSnapshot(t, group)
				if strings.Contains(snapshot, straggler) && hasReader(snapshot) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the peer never left a writer behind:\n%s", snapshot)
				}
				time.Sleep(20 * time.Millisecond)
			}

			// The wrapper alone. Signalling the group would kill the readers too and prove nothing
			// about who owns them.
			if err := syscall.Kill(group, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait() // WaitDelay bounds this even while a leaked reader holds the pipes open

			// Its readers must be gone with it — they cannot leave on their own here, because the
			// writer this peer left behind is still holding the pipe. That straggler is a separate
			// leak with its own owner, so the assertion is about the readers the CAPTURE owns, not
			// about an empty group.
			deadline = time.Now().Add(wait.Deadline)
			for hasReader(groupSnapshot(t, group)) {
				if time.Now().After(deadline) {
					t.Fatalf("a stopped capture left its reader behind:\n%s", groupSnapshot(t, group))
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// The drain budget is not a deadline on the peer. By the time it starts counting, the peer has been
// waited on and every writer Coop owns is closed, so a reader that has not finished is either
// blocked on a writer nobody told us about or simply not being scheduled — and POSIX sh cannot tell
// those apart. Five seconds decided that a reader which had not drained yet never would, and threw
// away replies the peer had already produced. This pins the behavior rather than the number: the
// readers are frozen for longer than the old budget and then released, and the reply still arrives.
func TestConsultWrapperSlowDrainKeepsTheReply(t *testing.T) {
	const reply = "CAPTURE_SURVIVED_THE_FREEZE"
	dir := consultStubDir(t)
	writeStub(t, dir, "gemini",
		`printf '{"type":"message","role":"assistant","content":"`+reply+`"}\n'
printf '{"type":"result","status":"success"}\n'`)

	cmd := consultWrapperCommand(t, dir, "")
	var output strings.Builder
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	group := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-group, syscall.SIGKILL) }()

	frozen := freezeReaders(t, group)
	if len(frozen) == 0 {
		t.Fatalf("no reader to freeze:\n%s", groupSnapshot(t, group))
	}
	// Longer than the budget that used to give up, so a reinstated five seconds fails here.
	time.Sleep(8 * time.Second)
	for _, pid := range frozen {
		_ = syscall.Kill(pid, syscall.SIGCONT)
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("a consult whose drain was merely slow failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), reply) {
		t.Fatalf("the reply the peer produced was discarded:\n%s", output.String())
	}
}

// A peer is its own client, so it runs on the box's PATH, not the lead's. A Codex lead prepends its
// vendored rg to every command it runs, and the pinned Gemini refuses an rg outside a trusted system
// directory without looking further down PATH: a consult from a Codex lead lost ripgrep.
func TestConsultPeerRunsOnTheBoxPath(t *testing.T) {
	dir := consultStubDir(t)
	lead := t.TempDir() // the lead client's own tool directory
	writeStub(t, lead, "rg", "exit 0")
	seen := filepath.Join(dir, "peer-path")
	writeStub(t, dir, "gemini", `printf '%s' "$PATH" >"`+seen+`"
printf '{"type":"message","role":"assistant","content":"OK"}\n'
printf '{"type":"result","status":"success"}\n'`)
	boxPath := dir + ":" + os.Getenv("PATH")
	cmd := consultWrapperCommand(t, dir, "")
	cmd.Env = append(cmd.Env, "PATH="+lead+":"+boxPath, "COOP_BOX_PATH="+boxPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("consult failed: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(seen); err != nil || string(got) != boxPath {
		t.Fatalf("the peer ran on PATH %q (%v), want the box's %q", got, err, boxPath)
	}
}

// freezeReaders stops every capture drain in pgid and returns them, so the caller can let them go
// again. Stopping the reader is the only portable way to make a drain slow without making it stuck.
func freezeReaders(t *testing.T, pgid int) []int {
	t.Helper()
	deadline := time.Now().Add(wait.Deadline)
	for {
		var frozen []int
		for _, row := range strings.Split(groupSnapshot(t, pgid), "\n") {
			if !isReaderRow(row) {
				continue
			}
			pid, err := strconv.Atoi(strings.Fields(row)[0])
			if err != nil {
				continue
			}
			if syscall.Kill(pid, syscall.SIGSTOP) == nil {
				frozen = append(frozen, pid)
			}
		}
		if len(frozen) > 0 || time.Now().After(deadline) {
			return frozen
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// consultStubDir is a scratch consult with the three commands the wrapper reaches for. The setsid
// stub is the important one: without it these tests only reach their interesting state on a host
// that HAS no setsid, because run() would otherwise put the peer in its own session, reap that
// group, and close the pipe for us — so the same test passed on macOS and spun for a minute on
// Linux. Stubbing it makes every host take the arm these tests describe: a peer whose leftover
// writer run() has no group to reach.
func consultStubDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "coop-consult"), []byte(ConsultWrapper()), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, dir, "timeout", "shift 3\nexec \"$@\"")
	writeStub(t, dir, "setsid", "exec \"$@\"")
	if err := os.MkdirAll(filepath.Join(dir, ".agent", "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func consultWrapperCommand(t *testing.T, dir, streamLimit string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(filepath.Join(dir, "coop-consult"), "advisor", "--fresh", "question")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"COOP_PEERS=gemini",
		"COOP_CONSULT_ADVISOR_TARGETS=gemini:test",
		"COOP_RUN_ID=",
		"COOP_CONSULT_TIMEOUT=",
		"COOP_CONSULT_STREAM_LIMIT="+streamLimit,
	)
	// Its own group, so "did anything survive?" is one question with one answer.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A leaked reader inherits the wrapper's stdio, so Wait would block on the copy long after the
	// wrapper itself is gone — the leak hiding the leak. Bound it, and read the process table for
	// the answer instead.
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// groupSnapshot lists what is still in pgid, command line included, so a failure names the
// survivor instead of asserting a count nobody can act on.
func groupSnapshot(t *testing.T, pgid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=,command=").Output()
	if err != nil {
		t.Fatalf("read the process table: %v", err)
	}
	group := strconv.Itoa(pgid)
	var rows []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[2] == group {
			rows = append(rows, strings.TrimSpace(line))
		}
	}
	return strings.Join(rows, "\n")
}

func hasReader(snapshot string) bool {
	for _, row := range strings.Split(snapshot, "\n") {
		if isReaderRow(row) {
			return true
		}
	}
	return false
}

// isReaderRow recognizes a capture's drain: a bare `cat` on the unlimited path — every other cat in
// the wrapper is given a path argument — and `dd bs=…` on the bounded one.
func isReaderRow(row string) bool {
	return strings.HasSuffix(row, " cat") || strings.Contains(row, " dd bs=")
}
