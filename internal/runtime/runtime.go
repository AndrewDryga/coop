// Package runtime locates and drives the container runtime — Apple's container,
// Docker, or Podman — behind a small surface the rest of the tool talks to.
package runtime

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
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
		// Validate the COOP_RUNTIME override here, not later with a misleading "image not built":
		// it must resolve on PATH, and an UNRECOGNIZED override (a typo'd path, /bin/false) must
		// also answer `--version`, so a non-runtime fails clearly. A known runtime (docker/podman/
		// container) is trusted on PATH alone, matching the auto-detect path below.
		if _, err := exec.LookPath(override); err != nil {
			return Runtime{}, fmt.Errorf("runtime %q not found (from COOP_RUNTIME) — install it, or unset COOP_RUNTIME to auto-detect", override)
		}
		if !isKnownRuntime(override) {
			if err := exec.Command(override, "--version").Run(); err != nil {
				return Runtime{}, fmt.Errorf("COOP_RUNTIME=%q isn't a usable container runtime (it didn't answer --version) — set docker, podman, or container", override)
			}
		}
		return Runtime{Name: override}, nil
	}
	for _, name := range []string{"container", "docker", "podman"} {
		if _, err := exec.LookPath(name); err == nil {
			return Runtime{Name: name}, nil
		}
	}
	return Runtime{}, errors.New("no container runtime found — install Apple 'container' (macOS 26), Docker, or Podman")
}

// isKnownRuntime reports whether name is one of the container runtimes coop drives, by its base
// name (so an absolute path like /usr/bin/docker still counts).
func isKnownRuntime(name string) bool {
	switch filepath.Base(name) {
	case "docker", "podman", "container":
		return true
	}
	return false
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

// psIDs returns the ids of running containers matching all the given ps --filter
// expressions (e.g. "label=coop=box"). Nil on error or no match.
func (r Runtime) psIDs(filters ...string) []string {
	args := []string{"ps", "-q"}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	out, err := exec.Command(r.Name, args...).Output()
	if err != nil {
		return nil
	}
	// Fields (not Split on "\n") so a blank line or a stray CRLF can't yield an empty/`\r`-suffixed
	// id — which would over-count CountByLabel or silently no-op a kill. Ids carry no whitespace.
	return strings.Fields(string(out))
}

// CountByLabel returns how many running containers carry the label key=value.
func (r Runtime) CountByLabel(key, value string) int {
	return len(r.psIDs("label=" + key + "=" + value))
}

// KillByLabel sends SIGKILL to every running container whose label matches
// key=value. Returns the number of containers killed.
func (r Runtime) KillByLabel(key, value string) int {
	n := 0
	for _, id := range r.psIDs("label=" + key + "=" + value) {
		if r.kill(id) {
			n++
		}
	}
	return n
}

func (r Runtime) kill(id string) bool {
	return id != "" && exec.Command(r.Name, "kill", id).Run() == nil
}

// RemoveContainer force-removes a container by id (or name) — `rm -f`, which stops it first.
// Used to tear down a supervised box by its deterministic --cidfile id even before its labels
// are queryable. Reports whether removal succeeded; a no-op (empty id) returns false.
func (r Runtime) RemoveContainer(id string) bool {
	return id != "" && exec.Command(r.Name, "rm", "-f", id).Run() == nil
}

// RemoveByLabel force-removes (`rm -f`: stop then delete) every running container whose label
// matches key=value, returning how many were removed. Like KillByLabel but it deletes rather
// than just kills — so a SIGKILL-orphaned `docker run --rm` container (whose run client was
// killed before it could auto-remove) is gone, not left lingering in Exited state.
func (r Runtime) RemoveByLabel(key, value string) int {
	n := 0
	for _, id := range r.psIDs("label=" + key + "=" + value) {
		if r.RemoveContainer(id) {
			n++
		}
	}
	return n
}

// SupportsCIDFile reports whether this runtime understands `docker run --cidfile` — docker and
// podman do; Apple's `container` CLI differs, so the supervisor falls back to labels there.
func (r Runtime) SupportsCIDFile() bool {
	return r.Name == "docker" || r.Name == "podman"
}
