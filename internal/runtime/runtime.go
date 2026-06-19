// Package runtime locates and drives the container runtime — Apple's container,
// Docker, or Podman — behind a small surface the rest of the tool talks to.
package runtime

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runtime is a resolved container CLI (e.g. "docker").
type Runtime struct {
	Name string
}

// Detect picks the runtime: an explicit override wins; otherwise the first of
// container, docker, podman found on PATH.
func Detect(override string) (Runtime, error) {
	if override != "" {
		return Runtime{Name: override}, nil
	}
	for _, name := range []string{"container", "docker", "podman"} {
		if _, err := exec.LookPath(name); err == nil {
			return Runtime{Name: name}, nil
		}
	}
	return Runtime{}, errors.New("no container runtime found — install Apple 'container' (macOS 26), Docker, or Podman")
}

// EnsureDaemon verifies the daemon is reachable. Only Docker exposes a daemon we
// probe up front; container and podman are checked lazily by their commands.
func (r Runtime) EnsureDaemon() error {
	if r.Name != "docker" {
		return nil
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		return errors.New("docker is installed but not running — start Docker Desktop and retry")
	}
	return nil
}

// Run executes the runtime with the given stdio and returns its exit code. A
// non-zero exit code comes back with a nil error (the command ran to
// completion); err is non-nil only when the process could not be started.
// A nil stream is treated as /dev/null by os/exec.
func (r Runtime) Run(stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	cmd := exec.Command(r.Name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, fmt.Errorf("%s: %w", r.Name, err)
}

// Silent reports whether an invocation succeeds, discarding its output. Used for
// existence probes like `image inspect` and `network inspect`.
func (r Runtime) Silent(args ...string) bool {
	return exec.Command(r.Name, args...).Run() == nil
}

// CountByLabel returns how many running containers carry the label key=value.
func (r Runtime) CountByLabel(key, value string) int {
	out, err := exec.Command(r.Name, "ps", "-q", "--filter", "label="+key+"="+value).Output()
	if err != nil {
		return 0
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// KillByLabel sends SIGKILL to every running container whose label matches
// key=value. Returns the number of containers killed.
func (r Runtime) KillByLabel(key, value string) int {
	out, err := exec.Command(r.Name, "ps", "-q", "--filter", "label="+key+"="+value).Output()
	if err != nil || len(out) == 0 {
		return 0
	}
	n := 0
	for _, id := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id != "" && exec.Command(r.Name, "kill", id).Run() == nil {
			n++
		}
	}
	return n
}
