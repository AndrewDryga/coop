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
	"time"

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
		!strings.Contains(GateSlotScript(), "\n\t\t\texport COOP_GATE_SLOT_HELD=1\n") || !strings.Contains(GateSlotScript(), "\n[ -z \"${COOP_GATE_SLOT_HELD-}\" ] || launch \"$@\"\n") {
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
		if !IsGateTmpfs(got, memory) {
			t.Errorf("IsGateTmpfs refuses its own value %q", got)
		}
	}
}

// A filtered box may ask for one tmpfs, and only the one coop plans for its memory limit, size
// included. Anything else, such as a far larger size, a wider mount or another path, would give the
// box memory or a place it was not meant to have.
func TestIsGateTmpfsAcceptsOnlyThePlannedFolder(t *testing.T) {
	const folder = GateTmpPath + ":rw,exec,nosuid,nodev,size="
	for _, tc := range []struct {
		name, value, memory string
		want                bool
	}{
		{"no limit", folder + "2g", "", true},
		{"a 4g box", folder + "1g", "4g", true},
		{"the minimum", folder + "512m", "1g", true},
		{"far larger than planned", folder + "99999g", "4g", false},
		{"far larger, no limit", folder + "99999g", "", false},
		{"slightly larger", folder + "1025m", "4g", false},
		{"planned for another limit", folder + "2g", "4g", false},
		{"no size", GateTmpPath + ":rw,exec,nosuid,nodev", "", false},
		{"a unit that is not a size", folder + "2t", "", false},
		{"more options", folder + "2g,uid=0", "", false},
		{"suid", GateTmpPath + ":rw,exec,suid,nodev,size=2g", "", false},
		{"no exec", GateTmpPath + ":rw,noexec,nosuid,nodev,size=2g", "", false},
		{"no options", GateTmpPath, "", false},
		{"another path", "/tmp:rw,exec,nosuid,nodev,size=2g", "", false},
		{"a home folder", "/home/node:rw,exec,nosuid,nodev,size=2g", "", false},
		{"a folder below it", GateTmpPath + "/sub:rw,exec,nosuid,nodev,size=2g", "", false},
		{"nothing", "", "", false},
	} {
		if got := IsGateTmpfs(tc.value, tc.memory); got != tc.want {
			t.Errorf("%s: IsGateTmpfs(%q, %q) = %t, want %t", tc.name, tc.value, tc.memory, got, tc.want)
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

// gateTmpHelper writes coop-gate-slot with its memory folder moved to tmp, which stands in for
// /tmp/coop-gate: a developer's computer has none, and a box's would be shared with the gate running
// these tests.
func gateTmpHelper(t *testing.T, tmp string) string {
	t.Helper()
	line := "tmp=" + GateTmpPath + "\n"
	if !strings.Contains(GateSlotScript(), line) {
		t.Fatalf("the script has no %q line to point at the test folder", line)
	}
	helper := filepath.Join(t.TempDir(), "coop-gate-slot")
	if err := os.WriteFile(helper, []byte(strings.Replace(GateSlotScript(), line, "tmp="+tmp+"\n", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	return helper
}

// gateTmpCommand is a gate under that helper with the given temp variables, as a gate inside a
// gate: COOP_GATE_SLOT_HELD makes it run at once, with no slot and no flock.
func gateTmpCommand(helper string, vars []string, gate ...string) *exec.Cmd {
	cmd := exec.Command(helper, append([]string{"--"}, gate...)...)
	cmd.Env = envWithout([]string{"TMPDIR", "GOTMPDIR", "COOP_GATE_SLOT_HELD"}, append(vars, "COOP_GATE_SLOT_HELD=1")...)
	return cmd
}

// endedWith is the status a command ended with, or -1 when it did not end on its own.
func endedWith(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	return -1
}

// A gate with no TMPDIR gets a temp folder of its own inside the memory folder, with TMPDIR and
// GOTMPDIR set to it, and the folder is gone when the gate ends, whatever its status. Two gates at
// once get two folders, so one cannot fill the other's.
func TestGateSlotGivesEachGateAFolderOfItsOwn(t *testing.T) {
	tmp := t.TempDir()
	helper := gateTmpHelper(t, tmp)
	show := `printf '%s|%s|' "$TMPDIR" "$GOTMPDIR"; if [ -w "$TMPDIR" ]; then echo writable; else echo no; fi; exit "$1"`
	for _, status := range []int{0, 3} {
		out, err := gateTmpCommand(helper, nil, "sh", "-c", show, "gate", strconv.Itoa(status)).Output()
		if code := endedWith(err); code != status {
			t.Fatalf("gate that exits %d ended with %d (%v): the helper must pass its status on", status, code, err)
		}
		parts := strings.Split(strings.TrimSpace(string(out)), "|")
		if len(parts) != 3 || parts[0] != parts[1] || filepath.Dir(parts[0]) != tmp || !strings.HasPrefix(filepath.Base(parts[0]), "run.") || parts[2] != "writable" {
			t.Fatalf("TMPDIR|GOTMPDIR|state in a gate that exits %d = %q, want one writable run.* folder in %s for both", status, out, tmp)
		}
		if _, err := os.Stat(parts[0]); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the folder of a gate that exited %d is still there after it: %v", status, err)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("the memory folder holds %d entries after every gate ended", len(left))
	}

	// Gate A keeps running until gate B has started and written its folder.
	scratch := t.TempDir()
	a, b, done := filepath.Join(scratch, "a"), filepath.Join(scratch, "b"), filepath.Join(scratch, "done")
	first := gateTmpCommand(helper, nil, "sh", "-c", `echo "$TMPDIR" >"$1"; while [ ! -e "$2" ]; do sleep 0.05; done`, "gate", a, done)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	wait.For(t, "the first gate to start", func() bool { data, _ := os.ReadFile(a); return strings.HasSuffix(string(data), "\n") })
	second := gateTmpCommand(helper, nil, "sh", "-c", `echo "$TMPDIR" >"$1"`, "gate", b)
	if out, err := second.CombinedOutput(); err != nil {
		t.Fatalf("second gate: %v\n%s", err, out)
	}
	if err := os.WriteFile(done, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatal(err)
	}
	one, _ := os.ReadFile(a)
	two, _ := os.ReadFile(b)
	if string(one) == "" || string(one) == string(two) {
		t.Errorf("two gates at once shared a temp folder: %q and %q", one, two)
	}
}

// A gate that already has a TMPDIR keeps it, and Go's temp folder with it, which follows it: a
// project that points its temp files at a bigger disk is left alone, and no folder is made. A gate
// that set only GOTMPDIR keeps that and gets the rest from the memory folder. An empty TMPDIR is none.
func TestGateSlotKeepsATempFolderTheGateAlreadyHas(t *testing.T) {
	const folder = "<a folder of its own>"
	for _, tc := range []struct {
		name         string
		vars         []string
		tmpdir, goTm string // what the gate sees: unset, the value it had, or folder
	}{
		{"TMPDIR set", []string{"TMPDIR=/big"}, "/big", "unset"},
		{"TMPDIR and GOTMPDIR set", []string{"TMPDIR=/big", "GOTMPDIR=/bigger"}, "/big", "/bigger"},
		{"only GOTMPDIR set", []string{"GOTMPDIR=/bigger"}, folder, "/bigger"},
		{"TMPDIR empty", []string{"TMPDIR="}, folder, folder},
		{"neither set", nil, folder, folder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			out, err := gateTmpCommand(gateTmpHelper(t, tmp), tc.vars, "sh", "-c", `printf '%s|%s' "${TMPDIR-unset}" "${GOTMPDIR-unset}"`).Output()
			if err != nil {
				t.Fatalf("gate under the helper: %v\n%s", err, out)
			}
			got := strings.Split(string(out), "|")
			if len(got) != 2 {
				t.Fatalf("gate printed %q", out)
			}
			madeFolder := false
			for i, want := range []string{tc.tmpdir, tc.goTm} {
				if want != folder {
					if got[i] != want {
						t.Errorf("temp variable %d in the gate = %q, want %q", i, got[i], want)
					}
					continue
				}
				madeFolder = true
				if filepath.Dir(got[i]) != tmp || !strings.HasPrefix(filepath.Base(got[i]), "run.") {
					t.Errorf("temp variable %d in the gate = %q, want a run.* folder in %s", i, got[i], tmp)
				}
			}
			// Nothing is made for a gate that keeps its TMPDIR, and what was made is gone with the gate.
			if left, _ := os.ReadDir(tmp); len(left) != 0 {
				t.Errorf("the memory folder holds %d entries after the gate ended (a folder was made: %t)", len(left), madeFolder)
			}
		})
	}
}

// Where the box has no usable memory folder, a gate keeps the temp folder it had: a box from before
// the folder existed, a runtime that mounts none, or a folder this user cannot write all still run
// the gate.
func TestGateSlotLeavesTheTempFolderAloneWithoutAUsableMemoryFolder(t *testing.T) {
	root := t.TempDir()
	readOnly := filepath.Join(root, "read-only")
	notAFolder := filepath.Join(root, "file")
	if err := os.Mkdir(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readOnly, 0o755) // so the test's temp folder can be removed
	if err := os.WriteFile(notAFolder, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dir string
		skip      bool
	}{
		{"no folder", filepath.Join(root, "missing"), false},
		{"a file where the folder should be", notAFolder, false},
		{"a folder the user cannot write", readOnly, os.Geteuid() == 0}, // root writes anywhere
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip {
				t.Skip("running as root")
			}
			out, err := gateTmpCommand(gateTmpHelper(t, tc.dir), []string{"TMPDIR="}, "sh", "-c", `printf '%s|%s' "${TMPDIR-unset}" "${GOTMPDIR-unset}"`).Output()
			if err != nil {
				t.Fatalf("gate under the helper: %v\n%s", err, out)
			}
			if string(out) != "|unset" {
				t.Errorf("TMPDIR|GOTMPDIR in the gate = %q, want its own empty TMPDIR and no GOTMPDIR", out)
			}
		})
	}
}

// A gate killed before it can clean up, as a timeout kills one, leaves its folder behind. The next
// gate removes such folders once they are hours old, and only run.* folders: a recent one may belong
// to a gate still running.
func TestGateSlotClearsTheFoldersOfGatesKilledHoursAgo(t *testing.T) {
	tmp := t.TempDir()
	helper := gateTmpHelper(t, tmp)
	marker := filepath.Join(t.TempDir(), "folder")
	// The gate kills the helper that runs it, so nothing is left to remove its folder.
	killed := gateTmpCommand(helper, nil, "sh", "-c", `echo "$TMPDIR" >"$1"; kill -KILL "$PPID"`, "gate", marker)
	if err := killed.Run(); err == nil {
		t.Fatal("the helper was not killed")
	}
	data, _ := os.ReadFile(marker)
	leftover := strings.TrimSpace(string(data))
	if filepath.Dir(leftover) != tmp {
		t.Fatalf("the killed gate's folder = %q, want one in %s", leftover, tmp)
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Fatalf("a killed gate's folder was removed with nobody to remove it: %v", err)
	}

	// Others the next gate meets: an old folder with something in it, an old file named like a folder,
	// an old entry that is not a run folder, and a recent run folder.
	old := time.Now().Add(-24 * time.Hour)
	staleDir, staleFile := filepath.Join(tmp, "run.stale"), filepath.Join(tmp, "run.file")
	notRun, recent := filepath.Join(tmp, "go-build-kept"), filepath.Join(tmp, "run.recent")
	for _, dir := range []string{staleDir, notRun, recent} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(staleDir, "left"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{staleDir, staleFile, notRun} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	nextGate := func() {
		t.Helper()
		if out, err := gateTmpCommand(helper, nil, "true").CombinedOutput(); err != nil {
			t.Fatalf("next gate: %v\n%s", err, out)
		}
	}
	// Minutes after the kill, the leftover may still be a running gate's: it stays.
	nextGate()
	for _, path := range []string{leftover, staleFile, notRun, recent} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed by a gate that started minutes after it was made: %v", filepath.Base(path), err)
		}
	}
	if _, err := os.Stat(staleDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a run folder a day old was kept: %v", err)
	}
	// Hours later it is not.
	if err := os.Chtimes(leftover, old, old); err != nil {
		t.Fatal(err)
	}
	nextGate()
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the killed gate's folder was kept a day after it: %v", err)
	}
	for _, path := range []string{staleFile, notRun, recent} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed, and is not an old run folder: %v", filepath.Base(path), err)
		}
	}
}
