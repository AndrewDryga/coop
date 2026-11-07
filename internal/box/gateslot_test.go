package box

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
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
		return strings.Contains(string(data), "Waiting for another gate to finish: 2 already running on this host (slots in "+slots+")")
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
	// This runtime is not Docker, whose flags the memory folder needs, so the gate keeps its temp folder.
	if strings.Contains(string(args), "--tmpfs") {
		t.Fatalf("a runtime that does not take Docker's flags was handed a tmpfs:\n%s", args)
	}
}

// A Docker box that carries the helper also gets the memory folder the helper points a gate at: a
// quarter of the box's memory limit, or 2g with none. A sign-in box has no helper and no folder.
func TestRunMountsTheGateTmpfsWhereTheGateHelperIs(t *testing.T) {
	const folder = GateTmpPath + ":rw,exec,nosuid,nodev,size="
	for _, tc := range []struct {
		name, memory string
		login        bool
		want         []string // every --tmpfs value in the run
	}{
		{"no memory limit", "", false, []string{folder + "2g"}},
		{"a 4g box", "4g", false, []string{folder + "1g"}},
		{"a 6g box", "6g", false, []string{folder + "1536m"}},
		{"a small box keeps the minimum", "1g", false, []string{folder + "512m"}},
		{"a sign-in box has neither", "4g", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(forkspace.TestExecutionRegistryRootEnv, t.TempDir())
			cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none", Memory: tc.memory}
			recorder := filepath.Join(t.TempDir(), "runtime-args")
			spec := RunSpec{Image: "i", Repo: t.TempDir(), Cmd: []string{"true"}, Batch: true, Quiet: true}
			if tc.login {
				spec.Agent, spec.Homes, spec.Login = "codex", true, true
			}
			if code, err := Run(cfg, dockerRecorder(t, recorder), spec); err != nil || code != 0 {
				t.Fatalf("Run = (%d, %v), want success", code, err)
			}
			var got []string
			args := recordedRun(t, recorder)
			for i, arg := range args {
				if arg == "--tmpfs" {
					got = append(got, args[i+1])
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("--tmpfs values = %q, want %q:\n%q", got, tc.want, args)
			}
			if hasHelper := slices.ContainsFunc(args, func(arg string) bool { return strings.HasSuffix(arg, ":"+GateSlotPath+":ro") }); hasHelper == tc.login {
				t.Errorf("gate slot helper mounted = %t in a box with login = %t", hasHelper, tc.login)
			}
		})
	}
}

// The folder is a quarter of the memory limit, as Docker reads the limit, and never under 512m.
func TestGateTmpfsIsAQuarterOfTheBoxMemory(t *testing.T) {
	const folder = GateTmpPath + ":rw,exec,nosuid,nodev,size="
	for memory, size := range map[string]string{
		"": "2g", "0": "2g", "lots": "2g", "9999p": "2g", // no limit, or none that can be read
		"4g": "1g", "4G": "1g", "4GiB": "1g", "4 gb": "1g", "8g": "2g", "10g": "2560m", "6g": "1536m",
		"4194304k": "1g", "4096m": "1g", "4294967296": "1g", "0.5t": "128g",
		"2g": "512m", "1g": "512m", "1.5G": "512m", "512m": "512m", "64m": "512m", "1": "512m", // the minimum
	} {
		got := gateTmpfs(memory)
		if got != folder+size {
			t.Errorf("gateTmpfs(%q) = %q, want size %s", memory, got, size)
		}
		if !IsGateTmpfs(got) {
			t.Errorf("IsGateTmpfs refuses its own value %q", got)
		}
	}
}

// A filtered box may ask for one tmpfs, and only in the shape coop makes. Anything wider, or at
// another path, would hide or widen something the box was not meant to touch.
func TestIsGateTmpfsAcceptsOnlyTheGatesFolder(t *testing.T) {
	const folder = GateTmpPath + ":rw,exec,nosuid,nodev,size="
	for value, want := range map[string]bool{
		folder + "2g": true, folder + "512m": true, folder + "1536m": true,
		folder + "0m": false, folder + "g": false, folder + "2": false, folder + "2g,uid=0": false, folder + "2t": false,
		GateTmpPath + ":rw,exec,suid,nodev,size=2g":       false,
		GateTmpPath + ":rw,noexec,nosuid,nodev,size=2g":   false,
		GateTmpPath + ":rw,exec,nosuid,nodev":             false,
		GateTmpPath:                                       false,
		"/tmp:rw,exec,nosuid,nodev,size=2g":               false,
		"/home/node:rw,exec,nosuid,nodev,size=2g":         false,
		GateTmpPath + "/sub:rw,exec,nosuid,nodev,size=2g": false,
		"": false,
	} {
		if got := IsGateTmpfs(value); got != want {
			t.Errorf("IsGateTmpfs(%q) = %t, want %t", value, got, want)
		}
	}
}

// The helper writes the same folder the run mounts.
func TestGateSlotScriptNamesTheGateTmpPath(t *testing.T) {
	if !strings.Contains(GateSlotScript(), "\ntmp="+GateTmpPath+"\n") {
		t.Fatalf("coop-gate-slot does not use %s", GateTmpPath)
	}
}

// envWithout returns the test's own environment less the named variables, plus the given
// assignments: a gate under coop-gate-slot has TMPDIR and GOTMPDIR set, and so may this test.
func envWithout(drop []string, set ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(drop, name) {
			env = append(env, kv)
		}
	}
	return append(env, set...)
}

// The gate runs with TMPDIR and GOTMPDIR at the memory folder when the box has it and can write to
// it. Otherwise the gate keeps the temp folder it had: a box from before the folder existed, a
// runtime that mounts none, or one whose folder this user cannot write all still run the gate.
func TestGateSlotPointsAGatesTempFilesAtTheMemoryFolder(t *testing.T) {
	root := t.TempDir()
	usable := filepath.Join(root, "usable")
	readOnly := filepath.Join(root, "read-only")
	notAFolder := filepath.Join(root, "file")
	for _, dir := range []string{usable, readOnly} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(notAFolder, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readOnly, 0o755)  // so the test's temp folder can be removed
	cannotWrite := os.Geteuid() != 0 // root writes anywhere, so a read-only folder proves nothing

	line := "tmp=" + GateTmpPath + "\n"
	for _, tc := range []struct {
		name, dir string
		tempFiles bool // whether the gate gets the folder
		skip      bool
	}{
		{"a folder the box can write", usable, true, false},
		{"no folder", filepath.Join(root, "missing"), false, false},
		{"a file where the folder should be", notAFolder, false, false},
		{"a folder the user cannot write", readOnly, false, !cannotWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip {
				t.Skip("running as root")
			}
			if !strings.Contains(GateSlotScript(), line) {
				t.Fatalf("the script has no %q line to point at the test folder", line)
			}
			helper := filepath.Join(t.TempDir(), "coop-gate-slot")
			script := strings.Replace(GateSlotScript(), line, "tmp="+tc.dir+"\n", 1)
			if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			// COOP_GATE_SLOT_HELD makes it a gate inside a gate, which runs at once: no slot, no flock.
			gate := exec.Command(helper, "--", "sh", "-c", `printf '%s|%s' "${TMPDIR-unset}" "${GOTMPDIR-unset}"`)
			gate.Env = envWithout([]string{"TMPDIR", "GOTMPDIR", "COOP_GATE_SLOT_HELD"}, "TMPDIR=/prior", "COOP_GATE_SLOT_HELD=1")
			out, err := gate.CombinedOutput()
			if err != nil {
				t.Fatalf("gate under the helper: %v\n%s", err, out)
			}
			want := "/prior|unset"
			if tc.tempFiles {
				want = tc.dir + "|" + tc.dir
			}
			if string(out) != want {
				t.Errorf("TMPDIR|GOTMPDIR in the gate = %q, want %q", out, want)
			}
		})
	}
}
