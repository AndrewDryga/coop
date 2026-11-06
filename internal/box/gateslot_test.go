package box

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestGateSlotScriptShellcheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	script := filepath.Join(t.TempDir(), "coop-gate-slot")
	if err := os.WriteFile(script, []byte(GateSlotScript()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sc, "--severity=warning", script).CombinedOutput(); err != nil {
		t.Errorf("shellcheck flagged coop-gate-slot:\n%s", out)
	}
}

// coop's own make check (tools/gateslot) and every other project's gate share one limit, so both
// take the same two slot files and mark the gates they run with the same variable: a gate inside
// either one runs in the outer gate's slot.
func TestGateSlotScriptKeepsTheGateslotToolsSlots(t *testing.T) {
	tool, err := os.ReadFile(filepath.Join("..", "..", "tools", "gateslot", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tool), "\nconst slots = 2\n") || !strings.Contains(string(tool), `"slot-%d"`) ||
		!strings.Contains(GateSlotScript(), "for slot in 0 1; do") || !strings.Contains(GateSlotScript(), `file="$dir/slot-$slot"`) {
		t.Fatal("coop-gate-slot and tools/gateslot no longer take the same two slot files")
	}
	if !strings.Contains(string(tool), "\nconst held = \"COOP_GATE_SLOT_HELD\"\n") ||
		!strings.Contains(GateSlotScript(), "\n\t\t\texport COOP_GATE_SLOT_HELD=1\n") || !strings.Contains(GateSlotScript(), "\n[ -z \"${COOP_GATE_SLOT_HELD-}\" ] || exec \"$@\"\n") {
		t.Fatal("coop-gate-slot and tools/gateslot no longer mark the gates they run with the same variable")
	}
}

// TestGateSlotFlockHelper stands in for util-linux `flock -n FD` when a gate slot test runs it as a
// child: it takes the same kernel lock on the descriptor it inherits from the script's shell, so the
// tests run on a host without flock (macOS) and still share a real lock with that shell.
func TestGateSlotFlockHelper(t *testing.T) {
	if os.Getenv("COOP_GATE_SLOT_FLOCK_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 3 || args[1] != "-n" {
		os.Exit(2)
	}
	fd, err := strconv.Atoi(args[2])
	if err != nil {
		os.Exit(2)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// holdGateSlot takes slot i as a running gate does, the way coop's own make check takes it
// (tools/gateslot), and returns the file that holds it.
func holdGateSlot(t *testing.T, slots string, i int) (*os.File, error) {
	t.Helper()
	if err := os.MkdirAll(slots, 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(slots, "slot-"+strconv.Itoa(i)), os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	t.Cleanup(func() { file.Close() })
	return file, nil
}

// gateSlotHost writes coop-gate-slot and a stand-in for util-linux flock to a bin folder, and
// returns it with the environment of a gate on a host whose HOME is home. The gate starts outside
// any other gate's slot, even when make check runs these tests inside its own.
func gateSlotHost(t *testing.T, home string) (string, []string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "coop-gate-slot"), []byte(GateSlotScript()), 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nexec " + strconv.Quote(self) + " -test.run=^TestGateSlotFlockHelper$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "flock"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "COOP_GATE_SLOT_FLOCK_HELPER=1", "COOP_GATE_SLOT_HELD=")
}

// A project that is not coop puts its own gate under the host's gate slots with one word. With both
// slots taken by running gates it says it waits and does not start, then runs once a slot frees,
// marked as running in one and without the lock descriptor, and its exit status comes back
// unchanged. Its slot is free afterwards.
func TestGateSlotHoldsAProjectGateUntilASlotIsFree(t *testing.T) {
	home := t.TempDir()
	bin, env := gateSlotHost(t, home)
	slots := filepath.Join(home, ".cache", "coop-gate", "slots")
	var running []*os.File
	for i := range 2 {
		file, err := holdGateSlot(t, slots, i)
		if err != nil {
			t.Fatal(err)
		}
		running = append(running, file)
	}

	scratch := t.TempDir()
	mark, stderr := filepath.Join(scratch, "ran"), filepath.Join(scratch, "stderr")
	errFile, err := os.Create(stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer errFile.Close()
	gate := exec.Command(filepath.Join(bin, "coop-gate-slot"), "--", "sh", "-c",
		`if [ -e /dev/fd/9 ]; then echo "the lock descriptor leaked"; fi; echo "ran $COOP_GATE_SLOT_HELD" >"$1"; exit 3`, "gate", mark)
	gate.Env = env
	var stdout bytes.Buffer
	gate.Stdout, gate.Stderr = &stdout, errFile
	if err := gate.Start(); err != nil {
		t.Fatal(err)
	}
	wait.For(t, "the project gate to wait for a slot", func() bool {
		data, _ := os.ReadFile(stderr)
		return strings.Contains(string(data), "Waiting for another gate to finish: 2 already run on this host (slots in "+slots+")")
	})
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the gate started before it held a slot: %v", err)
	}
	running[1].Close() // one of the running gates finishes
	err = gate.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("gate exit = %v, want the gate's own status 3", err)
	}
	data, _ := os.ReadFile(stderr)
	if ran, err := os.ReadFile(mark); err != nil || string(ran) != "ran 1\n" || stdout.String() != "" || !strings.Contains(string(data), "Got a gate slot after ") {
		t.Fatalf("gate ran = %q, %v; stdout %q; stderr %q", ran, err, stdout.String(), data)
	}
	if file, err := holdGateSlot(t, slots, 1); err != nil {
		t.Fatalf("the finished gate kept its slot: %v", err)
	} else {
		file.Close()
	}
}

// A gate inside a gate, such as a project gate that starts with coop-gate-slot under the fork merge
// gate's own, runs at once in the outer gate's slot. With one slot free the outer gate takes it, and
// the inner gate does not wait for another: two merges at once would each hold a slot and wait for
// each other forever.
func TestGateSlotRunsAGateInsideAGateInItsSlot(t *testing.T) {
	home := t.TempDir()
	bin, env := gateSlotHost(t, home)
	if _, err := holdGateSlot(t, filepath.Join(home, ".cache", "coop-gate", "slots"), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	defer cancel()
	helper := filepath.Join(bin, "coop-gate-slot")
	gate := exec.CommandContext(ctx, helper, helper, "sh", "-c", "exit 3")
	gate.Env = env
	// A gate that waits is stopped with the gate inside it, so a failure leaves neither running.
	gate.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	gate.Cancel = func() error { return syscall.Kill(-gate.Process.Pid, syscall.SIGKILL) }
	out, err := gate.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the gate inside a gate waited for a second slot until the test stopped it:\n%s", out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || strings.Contains(string(out), "Waiting for another gate") {
		t.Fatalf("gate exit = %v, want the inner gate's status 3 without a wait:\n%s", err, out)
	}
}

// Without flock the slots cannot be taken, and the project's checks still run instead of failing.
func TestGateSlotRunsTheGateWithoutASlotWhenFlockIsMissing(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "coop-gate-slot"), []byte(GateSlotScript()), 0o755); err != nil {
		t.Fatal(err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sh, filepath.Join(bin, "sh")); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin, "HOME=" + t.TempDir()}
	gate := exec.Command(filepath.Join(bin, "coop-gate-slot"), "sh", "-c", "exit 4")
	gate.Env = env
	out, err := gate.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 4 || !strings.Contains(string(out), "running the gate without one") {
		t.Fatalf("gate without flock = %v\n%s", err, out)
	}
	usage := exec.Command(filepath.Join(bin, "coop-gate-slot"))
	usage.Env = env
	if out, err := usage.CombinedOutput(); !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "usage: coop-gate-slot [--] <command>") {
		t.Fatalf("coop-gate-slot with no command = %v\n%s", err, out)
	}
}

// Every box carries the helper, read-only on PATH, with nothing else asked of the run.
func TestRunMountsTheGateSlotHelperInEveryBox(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	code, err := Run(cfg, recorderRuntime(t, recorder), RunSpec{Image: "i", Repo: t.TempDir(), Cmd: []string{"true"}, Batch: true, Quiet: true})
	if err != nil || code != 0 {
		t.Fatalf("Run = (%d, %v), want success", code, err)
	}
	args, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(args), ":"+GateSlotPath+":ro") != 1 {
		t.Fatalf("runtime args mount the gate slot helper %d times, want once:\n%s", strings.Count(string(args), ":"+GateSlotPath+":ro"), args)
	}
}
