package cli

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// sleepInhibitorCmd returns the argv for a system sleep inhibitor that holds its assertion until
// pid exits, for the given GOOS — or nil when coop knows no inhibitor for the platform, or the
// tool isn't on PATH. lookPath reports whether a command is available (exec.LookPath in
// production). Pure and platform-parameterized, so the choice is unit-tested without a real binary
// and the switch is the obvious place to add other platforms (e.g. systemd-inhibit on Linux).
func sleepInhibitorCmd(goos string, pid int, lookPath func(string) bool) []string {
	switch goos {
	case "darwin":
		// caffeinate ships with macOS, but honor lookPath in case it's been stripped. -i prevents
		// idle sleep (on battery and AC), -m keeps the disk from idle-sleeping, -s prevents system
		// sleep on AC power; -d is deliberately omitted so the display may still sleep. -w ties the
		// assertion to coop's own process, so it self-releases even on a SIGKILL where the defer
		// below never runs.
		if lookPath("caffeinate") {
			return []string{"caffeinate", "-i", "-m", "-s", "-w", strconv.Itoa(pid)}
		}
	}
	return nil
}

// armKeepAwake starts a sleep inhibitor for a loop's duration when COOP_CAFFEINATE is on and one
// is available, returning a stop func to release it (a no-op otherwise, so callers can always
// `defer armKeepAwake(cfg)()`). A `coop loop` runs unattended for hours; an overnight drain is
// pointless if the laptop idle-sleeps midway through it. Best-effort: a missing tool or a failed
// start leaves the loop running unchanged.
func armKeepAwake(cfg *config.Config) func() {
	if !cfg.Caffeinate {
		return func() {}
	}
	argv := sleepInhibitorCmd(runtime.GOOS, os.Getpid(), func(name string) bool {
		_, err := exec.LookPath(name)
		return err == nil
	})
	if argv == nil {
		return func() {}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return func() {} // best-effort — the loop doesn't depend on it
	}
	ui.Info("keeping the machine awake while the loop runs (%s) — COOP_CAFFEINATE=0 to disable", argv[0])
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // reap; -w already releases the assertion if this never runs
	}
}
