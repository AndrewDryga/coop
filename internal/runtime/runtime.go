// Package runtime locates and drives the container runtime — Apple's container,
// Docker, or Podman — behind a small surface the rest of the tool talks to.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// killGrace is how long a canceled RunInterruptible waits after SIGTERM for the box to exit
// before escalating to SIGKILL — matches `coop fork stop`'s grace.
const killGrace = 3 * time.Second

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
		return errors.New("docker is installed but its daemon isn't responding — start it (Docker Desktop, or `systemctl start docker` on Linux) and retry")
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
	return exitCode(r.Name, cmd.Run())
}

// contextCommand makes a context deadline tear down the runtime CLI's whole process group, not
// just its direct process. Runtime wrappers may spawn helpers; leaving one alive would turn a
// bounded cleanup into a process leak. WaitDelay is the final backstop if a killed child wedges.
func contextCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	return cmd
}

func commandOutputError(err error, output []byte) error {
	if len(output) == 0 {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			output = exitErr.Stderr
		}
	}
	if detail := strings.TrimSpace(string(output)); detail != "" {
		return fmt.Errorf("%w: %s", err, detail)
	}
	return err
}

// RunInterruptible is Run for a cancelable box. The child runs in its OWN process group, so a
// Ctrl-C the terminal delivers to coop's foreground group does NOT reach it — that's what lets
// the loop's first Ctrl-C be a soft stop that finishes the current iteration. Canceling ctx (the
// second Ctrl-C) tears the box down: SIGTERM first — which a runtime forwards to the container so
// `--rm` removes it (a bare SIGKILL would orphan it, see box.assembleArgs) — a short grace, then
// SIGKILL. The signal targets the whole group (-pid) so no runtime helper is left behind.
func (r Runtime) RunInterruptible(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	cmd := exec.Command(r.Name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("%s: %w", r.Name, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		killGroup(cmd.Process.Pid, done)
		return -1, ctx.Err()
	case err := <-done:
		return exitCode(r.Name, err)
	}
}

// killGroup tears down the process group led by pid: SIGTERM, a short grace to exit, then SIGKILL
// if it's still alive. It signals the whole group (-pid), falling back to the bare pid, and drains
// done so the child is always reaped (no zombie). Mirrors `coop fork stop`.
func killGroup(pid int, done <-chan error) {
	signalGroup(pid, syscall.SIGTERM)
	select {
	case <-done:
		return
	case <-time.After(killGrace):
	}
	signalGroup(pid, syscall.SIGKILL)
	<-done
}

// signalGroup sends sig to the whole process group led by pid (-pid), falling back to the bare
// pid if the group send fails (e.g. the leader already exited).
func signalGroup(pid int, sig syscall.Signal) {
	if syscall.Kill(-pid, sig) != nil {
		_ = syscall.Kill(pid, sig)
	}
}

// exitCode maps a cmd.Run/Wait error to coop's convention: a clean exit is (0, nil), a non-zero
// exit is (code, nil) — the command ran to completion — and a start/other failure is (-1, err).
func exitCode(name string, err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, fmt.Errorf("%s: %w", name, err)
}

// Silent reports whether an invocation succeeds, discarding its output. Used for
// existence probes like `image inspect` and `network inspect`.
func (r Runtime) Silent(args ...string) bool {
	return exec.Command(r.Name, args...).Run() == nil
}

// psIDs returns the ids of running containers matching all the given ps --filter expressions
// (e.g. "label=coop=box"). Legacy best-effort callers treat an error like no match.
func (r Runtime) psIDs(filters ...string) []string {
	ids, _ := r.psIDsContext(context.Background(), filters...)
	return ids
}

// psIDsContext is the error-reporting, cancelable form used when cleanup must not claim success
// after a failed query. A real no-match is an empty slice with a nil error.
func (r Runtime) psIDsContext(ctx context.Context, filters ...string) ([]string, error) {
	if filepath.Base(r.Name) == "container" {
		return r.appleContainerIDsContext(ctx, filters...)
	}
	args := []string{"ps", "-q"}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	out, err := contextCommand(ctx, r.Name, args...).Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("run: %s %s: %w", r.Name, strings.Join(args, " "), commandOutputError(err, nil))
	}
	// Fields (not Split on "\n") so a blank line or a stray CRLF can't yield an empty/`\r`-suffixed
	// id — which would over-count CountByLabel or silently no-op a kill. Ids carry no whitespace.
	return strings.Fields(string(out)), nil
}

// appleContainerIDsContext applies Docker-style exact label filters to Apple container's JSON
// listing. Its CLI has no `ps --filter`; `list` defaults to running containers, and the current
// structured resource shape stores ids at the top level and labels under configuration.
func (r Runtime) appleContainerIDsContext(ctx context.Context, filters ...string) ([]string, error) {
	type item struct {
		ID            string `json:"id"`
		Configuration struct {
			ID     string            `json:"id"`
			Labels map[string]string `json:"labels"`
		} `json:"configuration"`
	}
	labels := make(map[string]string, len(filters))
	for _, filter := range filters {
		label, ok := strings.CutPrefix(filter, "label=")
		if !ok {
			return nil, fmt.Errorf("filter %q is unsupported on Apple container", filter)
		}
		key, value, ok := strings.Cut(label, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid label filter %q", filter)
		}
		labels[key] = value
	}
	out, err := contextCommand(ctx, r.Name, "list", "--format", "json").Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("run: %s list --format json: %w", r.Name, commandOutputError(err, nil))
	}
	var items []item
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("decode Apple container list: %w", err)
	}
	var ids []string
	for _, item := range items {
		match := true
		for key, value := range labels {
			if item.Configuration.Labels[key] != value {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		id := item.ID
		if id == "" {
			id = item.Configuration.ID
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
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
// matches key=value. It distinguishes no-match from query/removal failure so a caller never reports
// successful cleanup while a known container may remain. The caller owns the overall deadline.
func (r Runtime) RemoveByLabel(ctx context.Context, key, value string) (int, error) {
	ids, err := r.psIDsContext(ctx, "label="+key+"="+value)
	if err != nil {
		return 0, fmt.Errorf("list matching containers: %w", err)
	}
	n := 0
	for _, id := range ids {
		out, err := contextCommand(ctx, r.Name, "rm", "-f", id).CombinedOutput()
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return n, fmt.Errorf("run: %s rm -f %s: %w", r.Name, id, commandOutputError(err, out))
		}
		n++
	}
	return n, nil
}

// SupportsCIDFile reports whether this runtime understands `docker run --cidfile` — docker and
// podman do; Apple's `container` CLI differs, so the supervisor falls back to labels there.
func (r Runtime) SupportsCIDFile() bool {
	return r.Name == "docker" || r.Name == "podman"
}
