package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

type DockerCreate struct {
	Ref              DockerRef
	Image            string // immutable local sha256 ID; never pull during launch
	Options, Command []string
}

// ErrDockerCreateNotAttempted proves this create call submitted no daemon request.
// Only creation methods produce it; a failed post-create inspection never does.
var ErrDockerCreateNotAttempted = errors.New("Docker creation was not attempted")

var errDockerStartupProbe = errors.New("attached Docker startup observation unavailable")

const dockerImageFormat = `{"ID":{{json .Id}},"Labels":{{json (index .Config "Labels")}}}`

func (d *Docker) Image(ctx context.Context, name string) (string, map[string]string, error) {
	if !dockerToken(name, 512) || strings.HasPrefix(name, "-") {
		return "", nil, errors.New("invalid Docker image reference")
	}
	if err := d.Verify(ctx); err != nil {
		return "", nil, err
	}
	// Docker omits Config.Labels entirely for ordinary unlabelled images;
	// direct .Config.Labels fails its missingkey template policy before JSON.
	data, err := d.output(ctx, 64<<10, "image", "inspect", "--format", dockerImageFormat, name)
	if err != nil {
		return "", nil, err
	}
	var image struct {
		ID     string
		Labels map[string]string
	}
	if json.Unmarshal(data, &image) != nil || !strings.HasPrefix(image.ID, "sha256:") || !dockerHexID(strings.TrimPrefix(image.ID, "sha256:")) || len(image.Labels) > 256 {
		return "", nil, errors.New("invalid Docker image observation")
	}
	return image.ID, image.Labels, nil
}

// Only the already-composed workload arguments cross this finite grammar. The
// box layer separately validates mounts and verifies its role's security
// contract before start. Name, image, restart and auto-remove are never options.
func validDockerCreateOptions(options []string) bool {
	if len(options) > 4096 {
		return false
	}
	for i := 0; i < len(options); i++ {
		switch options[i] {
		case "--init", "--read-only", "-i", "-t", "-it":
		case "--mount", "-v", "-e", "--env-file", "-w", "--workdir", "--memory", "--cpus", "--pids-limit", "--cap-drop", "--cap-add", "--security-opt", "--network", "--user", "--log-driver", "--log-opt", "--tmpfs", "--entrypoint", "--label", "-p":
			i++
			if i >= len(options) || len(options[i]) > 64<<10 || strings.ContainsRune(options[i], '\x00') {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (d *Docker) CreateContainer(ctx context.Context, spec DockerCreate) (string, error) {
	if !spec.Ref.valid(false) || spec.Ref.ID != "" || !strings.HasPrefix(spec.Image, "sha256:") || !dockerHexID(strings.TrimPrefix(spec.Image, "sha256:")) || !validDockerCreateOptions(spec.Options) {
		return "", errors.Join(ErrDockerCreateNotAttempted, errors.New("invalid Docker creation intention"))
	}
	if err := d.VerifyLaunch(ctx); err != nil {
		return "", errors.Join(ErrDockerCreateNotAttempted, err)
	}
	_, present, err := d.InspectContainer(ctx, spec.Ref)
	if err != nil || present {
		return "", errors.Join(ErrDockerCreateNotAttempted, errors.New("Docker container creation requires a fresh intention"), err)
	}
	args := append([]string{"container", "create", "--pull", "never", "--restart", "no", "--name", spec.Ref.Name}, spec.Options...)
	args = append(args, dockerLabelArgs(spec.Ref.Labels)...)
	args = append(args, spec.Image)
	args = append(args, spec.Command...)
	data, err := d.output(ctx, 1024, args...)
	if err != nil {
		if errors.Is(err, errDockerCommandNotStarted) {
			err = errors.Join(ErrDockerCreateNotAttempted, err)
		}
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !dockerHexID(id) {
		return "", errors.New("Docker container create outcome is invalid")
	}
	ref := spec.Ref
	ref.ID = id
	value, present, err := d.InspectContainer(ctx, ref)
	if err != nil || !present || value.Image != spec.Image || value.AutoRemove || value.RestartPolicy != "no" || value.State.Status != "created" || !value.State.StartedAt.IsZero() {
		return id, errors.Join(errors.New("Docker container create outcome requires reconciliation"), err)
	}
	return id, nil
}

func (d *Docker) beforeStart(ctx context.Context, ref DockerRef) error {
	if err := d.VerifyLaunch(ctx); err != nil {
		return err
	}
	value, present, err := d.InspectContainer(ctx, ref)
	if err != nil {
		return err
	}
	if ref.ID == "" || !present || value.State.Status != "created" || value.State.Running || value.State.Paused || !value.State.StartedAt.IsZero() || value.RestartCount != 0 || value.AutoRemove || value.RestartPolicy != "no" {
		return errors.New("Docker start requires an exact never-started container")
	}
	return nil
}

func (d *Docker) StartContainer(ctx context.Context, ref DockerRef) error {
	if err := d.beforeStart(ctx, ref); err != nil {
		return err
	}
	if _, err := d.output(ctx, 1024, "container", "start", ref.ID); err != nil {
		return err
	}
	value, present, err := d.InspectContainer(ctx, ref)
	if err != nil || !present || value.State.StartedAt.IsZero() || value.RestartCount != 0 {
		return errors.Join(errors.New("Docker startup observation unavailable"), err)
	}
	return nil
}

// StartAttached attaches before the daemon starts the workload. onStarted is
// called once, only after daemon evidence, including a fast-exited workload. A
// client error/cancel is not proof of container death: caller owns exact teardown.
func (d *Docker) StartAttached(ctx context.Context, ref DockerRef, stdin io.Reader, stdout, stderr io.Writer, onStarted func() error) (int, error) {
	if err := d.beforeStart(ctx, ref); err != nil {
		return -1, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"--config", d.clientConfig, "--host", d.endpoint, "container", "start", "--attach"}
	if stdin != nil {
		args = append(args, "--interactive")
	}
	cmd := exec.Command(d.binary, append(args, ref.ID)...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = slices.Clone(d.env), stdin, stdout, stderr
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := runInterruptibleCommand(ctx, cmd)
		done <- result{code, err}
	}()
	started := false
	startDeadline := time.Now().Add(15 * time.Second)
	observe := func() error {
		if started {
			return nil
		}
		if time.Now().After(startDeadline) {
			return errors.New("Docker workload startup exceeded its deadline; outcome unknown")
		}
		probe, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		value, present, err := d.InspectContainer(probe, ref)
		if err != nil {
			return errors.Join(errDockerStartupProbe, err)
		}
		if !present {
			return errors.New("attached Docker container is confirmed absent")
		}
		if value.RestartCount != 0 {
			return errors.New("attached Docker container restarted outside its epoch")
		}
		if !value.State.StartedAt.IsZero() {
			started = true
			if onStarted != nil {
				return onStarted()
			}
		}
		return nil
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case outcome := <-done:
			if ctx.Err() != nil {
				return -1, errors.Join(ctx.Err(), outcome.err)
			}
			startupErr := observe()
			if ctx.Err() != nil {
				return -1, errors.Join(ctx.Err(), outcome.err, startupErr)
			}
			probe, stop := context.WithTimeout(ctx, 2*time.Second)
			value, present, err := d.InspectContainer(probe, ref)
			stop()
			if err != nil || !present || value.State.Running || value.State.Paused || !slices.Contains([]string{"exited", "dead"}, value.State.Status) || value.State.StartedAt.IsZero() || value.RestartCount != 0 {
				return -1, errors.Join(outcome.err, startupErr, err, errors.New("Docker attachment ended without a confirmed workload outcome; cleanup remains required"))
			}
			if ctx.Err() != nil {
				return -1, errors.Join(ctx.Err(), outcome.err, startupErr)
			}
			// This exact terminal observation supersedes only a transient probe
			// failure, never a callback failure or a confirmed invalid transition.
			if !started && errors.Is(startupErr, errDockerStartupProbe) {
				started, startupErr = true, nil
				if onStarted != nil {
					startupErr = onStarted()
				}
			}
			if !started {
				startupErr = errors.Join(startupErr, errors.New("Docker client exited without workload startup evidence"))
			}
			outcome.err = errors.Join(outcome.err, startupErr)
			if ctx.Err() != nil {
				return -1, errors.Join(ctx.Err(), outcome.err)
			}
			// A detached/failed client is not the workload. Only the daemon's
			// terminal state supplies the provider exit code.
			outcome.code = value.State.ExitCode
			return outcome.code, outcome.err
		case <-ticker.C:
			if err := observe(); err != nil {
				if errors.Is(err, errDockerStartupProbe) && ctx.Err() == nil && time.Now().Before(startDeadline) {
					continue // bounded retry; slow daemon observation is not workload failure
				}
				cancel()
				outcome := <-done
				return -1, errors.Join(err, outcome.err)
			}
		}
	}
}

func (d *Docker) StopContainer(ctx context.Context, ref DockerRef, graceSeconds int) error {
	if ref.ID == "" || graceSeconds < 0 || graceSeconds > 10 {
		return errors.New("invalid exact Docker stop")
	}
	value, present, err := d.InspectContainer(ctx, ref)
	if err != nil || !present {
		return err
	}
	if !value.State.Running && !value.State.Paused && !slices.Contains([]string{"restarting", "removing"}, value.State.Status) {
		return nil
	}
	_, stopErr := d.output(ctx, 1024, "container", "stop", "--timeout", strconv.Itoa(graceSeconds), ref.ID)
	value, present, err = d.InspectContainer(ctx, ref)
	if err == nil && (!present || !value.State.Running && !value.State.Paused && slices.Contains([]string{"exited", "dead"}, value.State.Status)) {
		return nil
	}
	return errors.Join(errors.New("Docker workload stop remains unconfirmed"), stopErr, err)
}

func (d *Docker) RemoveContainer(ctx context.Context, ref DockerRef) error {
	if ref.ID == "" {
		return errors.New("Docker removal requires immutable container ID")
	}
	_, present, err := d.InspectContainer(ctx, ref)
	if err != nil || !present {
		return err
	}
	_, removeErr := d.output(ctx, 1024, "container", "rm", "--force", ref.ID)
	absent, err := d.containerAbsent(ctx, ref)
	if err == nil && absent {
		return nil
	}
	return errors.Join(errors.New("Docker container cleanup remains pending"), removeErr, err)
}

// ExecRead and CopyArchive return bounded private evidence. Callers supply a
// release-owned command/path, never a request's executable or extraction target.
func (d *Docker) ExecRead(ctx context.Context, ref DockerRef, limit int, command ...string) ([]byte, error) {
	value, present, err := d.InspectContainer(ctx, ref)
	if err != nil || ref.ID == "" || !present || !value.State.Running || value.State.Paused || len(command) == 0 {
		return nil, errors.Join(errors.New("Docker evidence source unavailable"), err)
	}
	return d.output(ctx, limit, append([]string{"container", "exec", ref.ID}, command...)...)
}

func (d *Docker) CopyArchive(ctx context.Context, ref DockerRef, source string, limit int) ([]byte, error) {
	_, present, err := d.InspectContainer(ctx, ref)
	if err != nil || ref.ID == "" || !present || !strings.HasPrefix(source, "/") || strings.ContainsAny(source, "\x00\r\n") {
		return nil, errors.Join(errors.New("Docker retained evidence source unavailable"), err)
	}
	return d.output(ctx, limit, "container", "cp", ref.ID+":"+source, "-")
}
