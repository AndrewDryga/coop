package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/taskchannel"
)

// TaskToolServer serves one MCP session per client connection on the task channel. It is
// internal/taskmcp's Server, built by the caller that owns the queue — box only carries it.
type TaskToolServer interface {
	Serve(ctx context.Context, rw io.ReadWriter) error
}

// LabelTaskChannel is the LabelKey value on the task channel's helper container and volume, so
// neither is ever mistaken for an agent box by the sweeps that reap coop=box.
const LabelTaskChannel = "task-channel"

// taskChannelReadyTimeout bounds the helper's start: a cold image start takes seconds, never a
// minute — past that the runtime is wedged and the iteration must not launch a box that was
// promised tools it cannot reach.
const taskChannelReadyTimeout = 60 * time.Second

// taskChannel is the run-private channel a box reaches its task queue through.
//
// A unix socket created on the host is useless to the box: a bind-mounted socket crosses the
// VM boundary as an inode, not an endpoint (connect() is refused on OrbStack; see the KB card
// in-box-task-channel). So the listener runs where the box's kernel is — a helper container from
// the box's own image, --network none, root only inside its own namespace, holding nothing but a
// run-private named volume and the read-only multiplexer script. The box mounts that volume
// read-only and its MCP clients connect with `socat STDIO UNIX-CONNECT:`. The helper's stdio is
// coop's end: taskmcp multiplexes every client connection over it, and coop serves each one.
// The helper lives in its own pid namespace, so coop-entry's descendant drain never sees it.
type taskChannel struct {
	rt       runtime.Runtime
	volume   string
	name     string
	helper   *runtime.Helper
	stderr   *tailBuffer
	cancel   context.CancelFunc
	serving  chan error
	once     sync.Once
	closeErr error
}

// taskToolsInstructionNote joins the box briefing whenever the box serves task tools, so its agent
// finds them before it moves a task folder by hand or asks a person to run coop on the host.
const taskToolsInstructionNote = "\n# Tasks (coop task tools)\n" +
	"This box serves coop's task tools (the coop-tasks MCP server). Change the state of this\n" +
	"project's tasks only through them: the coop CLI is not installed here, and task folders are\n" +
	"never moved by hand.\n"

// NewTaskChannelVolume names a run-private channel volume; the same nonce names the helper container.
func NewTaskChannelVolume() string { return taskChannelVolumePrefix + randid.Hex(8) }

const taskChannelVolumePrefix = "coop-tasks-"

// ValidTaskChannelVolume reports whether name is one NewTaskChannelVolume could have made, so a name
// handed in from outside the run can only ever select coop's own channel volume.
func ValidTaskChannelVolume(name string) bool {
	nonce, ok := strings.CutPrefix(name, taskChannelVolumePrefix)
	return ok && len(nonce) == 16 && strings.Trim(nonce, "0123456789abcdef") == ""
}

// taskChannelMount is the box-side bind: the channel volume, read-only, at the socket's directory.
func taskChannelMount(volume string) string {
	return volume + ":" + taskchannel.BoxSocketDir + ":ro"
}

// ReapOrphanTaskChannels removes the task-channel volumes of workspace whose supervising host process
// is provably gone, judged as ReapOrphanBoxes judges a box (supervisorProvablyDead). A run closes its
// own channel on the way out; one whose coop was killed first — a replaced editor child, a closed
// terminal, a SIGKILL — leaves the volume behind, while its helper exits with its client. A volume
// something still mounts is refused by the runtime and left for a later sweep, and a volume with no
// owner label (a coop that predates it, or one that could not name itself) is never touched.
func ReapOrphanTaskChannels(ctx context.Context, rt runtime.Runtime, workspace string) (int, error) {
	host, _ := thisHost()
	volumes, err := rt.VolumeLabels(ctx, LabelKey, LabelTaskChannel, LabelHost)
	if err != nil {
		return 0, err
	}
	scope := workspaceScope(workspace)
	removed := 0
	for _, volume := range volumes {
		ref, ok := parseSupervisorLabel(volume.Labels[LabelHost])
		if !ok || ref.scope != scope || !supervisorProvablyDead(ref, host) {
			continue
		}
		// A refusal is a volume still mounted, or one another sweep just took: either way it is not
		// this sweep's to report, and the next one judges it again.
		if rt.RemoveVolume(ctx, volume.Name) == nil {
			removed++
		}
	}
	return removed, nil
}

// startTaskChannel creates the volume, launches the helper on it, and serves task tools over the
// helper's stdio until close. It returns once the helper reports its socket bound, so the box
// launched next finds the socket already listening. owner is the volume's LabelHost value, the host
// process whose death makes it an orphan (ReapOrphanTaskChannels); "" leaves it unlabeled, and so
// never swept, as a box with no provable supervisor is.
func startTaskChannel(ctx context.Context, rt runtime.Runtime, image, volume, runID, owner string, server TaskToolServer, script string) (*taskChannel, error) {
	create := []string{"volume", "create", "--label", LabelKey + "=" + LabelTaskChannel}
	if owner != "" {
		create = append(create, "--label", LabelHost+"="+owner)
	}
	if !rt.Silent(append(create, volume)...) {
		return nil, fmt.Errorf("task channel: could not create volume %s", volume)
	}
	c := &taskChannel{rt: rt, volume: volume, name: volume, stderr: &tailBuffer{max: 8 << 10}, serving: make(chan error, 1)}
	args := []string{"run", "--rm", "-i", "--network", "none", "--user", "root",
		"--name", c.name, "--label", LabelKey + "=" + LabelTaskChannel}
	if runID != "" {
		// The loop's cancel backstop removes everything carrying its run id.
		args = append(args, "--label", LabelRun+"="+runID)
	}
	if rt.SupportsRunLimits() {
		args = append(args, "--cap-drop", "ALL", "--security-opt", "no-new-privileges")
	}
	args = append(args, "-v", volume+":"+taskchannel.BoxSocketDir, "-v", script+":/coop/mux.js:ro",
		"--entrypoint", "node", image, "/coop/mux.js", taskchannel.BoxSocketPath)
	helper, err := rt.StartHelper(c.stderr, args...)
	if err != nil {
		_ = rt.Silent("volume", "rm", volume)
		return nil, fmt.Errorf("task channel: start helper: %w", err)
	}
	c.helper = helper
	serveCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	ready := make(chan struct{})
	go func() {
		c.serving <- taskchannel.ServeMux(serveCtx, helper.Stdout, helper.Stdin, func() { close(ready) }, server.Serve)
	}()
	timer := time.NewTimer(taskChannelReadyTimeout)
	defer timer.Stop()
	select {
	case <-ready:
		return c, nil
	case err := <-c.serving:
		c.serving <- err
		// The stream ended, so the helper is exiting — but its stderr is copied by the process
		// wait, which may not have run yet; the detail is worth a moment's patience.
		select {
		case <-helper.Exited():
		case <-time.After(2 * time.Second):
		}
		return nil, errors.Join(fmt.Errorf("task channel: helper exited before its socket was ready%s", c.stderrDetail()), c.close())
	case <-timer.C:
		return nil, errors.Join(fmt.Errorf("task channel: helper did not report ready within %s%s", taskChannelReadyTimeout, c.stderrDetail()), c.close())
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), c.close())
	}
}

func (c *taskChannel) stderrDetail() string {
	if detail := strings.TrimSpace(c.stderr.String()); detail != "" {
		return ": " + detail
	}
	return ""
}

// close stops serving, stops the helper (EOF on its stdin, then the interruptible teardown),
// and removes the volume. Idempotent; the box that mounted the volume must be gone by now or the
// runtime refuses the removal and says so.
func (c *taskChannel) close() error {
	c.once.Do(func() {
		var errs []error
		if c.helper != nil {
			defer c.helper.Stdout.Close() // after the serving join, including its bounded timeout
			errs = append(errs, c.helper.Close())
		}
		if c.cancel != nil {
			c.cancel()
		}
		select {
		case err := <-c.serving:
			if err != nil && !errors.Is(err, context.Canceled) {
				errs = append(errs, err)
			}
		case <-time.After(5 * time.Second):
			errs = append(errs, errors.New("task channel: serving loop did not stop"))
		}
		// The helper is gone, but the runtime may still be tearing down the box that mounted
		// the volume; a short retry covers that ordinary overlap.
		removed := false
		for attempt := 0; attempt < 20 && !removed; attempt++ {
			if removed = c.rt.Silent("volume", "rm", c.volume); !removed {
				time.Sleep(250 * time.Millisecond)
			}
		}
		if !removed {
			errs = append(errs, fmt.Errorf("task channel: volume %s was not removed — remove it by hand: %s volume rm %s", c.volume, c.rt.Name, c.volume))
		}
		c.closeErr = errors.Join(errs...)
	})
	return c.closeErr
}

// tailBuffer keeps the last max bytes written to it: enough of a helper's stderr to explain a
// failure, never the whole stream.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > t.max {
		excess := t.buf.Len() - t.max
		t.buf.Next(excess)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

// writeTaskChannelScript writes the multiplexer for the helper to run, world-readable: the helper
// reads it as a uid that does not own the host file.
func writeTaskChannelScript(artifacts compositionArtifactOps) (string, error) {
	path, err := artifacts.writeFile(artifacts.parent, taskchannel.MuxScript)
	if err != nil {
		return "", fmt.Errorf("task channel: write helper script: %w", err)
	}
	if err := artifacts.chmod(path, 0o644); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("task channel: make helper script readable: %w", err)
	}
	return path, nil
}
