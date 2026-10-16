package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The box announces itself by creating a file in the mounted workspace, written relative to the
// box's workdir: the host path and the in-box path are not the same string, and only one of them
// exists on each side. The host watches for it, so the instant is the host's own.
const (
	readyFile  = ".coop-bench-ready"
	readyProbe = ": > ./" + readyFile
)

// cancelTimeout is how long coop may take to exit after a person's Ctrl-C.
const cancelTimeout = 90 * time.Second

// stopStep is one signal of a stop and how long the launch gets to leave after it.
type stopStep struct {
	signal syscall.Signal
	grace  time.Duration
}

// stopSteps end a launch the way a person's terminal would, escalating only if it refuses. SIGKILL
// first is how this tool used to lose a filtered launch's gateway containers and volumes: coop was
// killed mid-teardown and the resources it had not reached yet stayed.
var stopSteps = []stopStep{{syscall.SIGINT, 20 * time.Second}, {syscall.SIGTERM, 10 * time.Second}, {syscall.SIGKILL, killGrace}}

const killGrace = 5 * time.Second

// bench is one measuring run: the coop it measures, where, and how it runs things. The how is
// fields, so a test can stand in a fake runtime, a stand-in coop and short bounds.
type bench struct {
	coop, repo, runtime      string
	env                      []string              // what every launch inherits
	command                  commandFunc           // runtime queries and metadata probes
	start                    func(*exec.Cmd) error // starts a launch
	token                    func() string         // names one sample's launches
	log                      io.Writer
	caseTimeout, goneTimeout time.Duration
	stopSteps                []stopStep
}

// getenv is key's value in the bench's environment, the last one as exec.Cmd reads it.
func (b *bench) getenv(key string) string {
	value := ""
	for _, entry := range b.env {
		if name, v, ok := strings.Cut(entry, "="); ok && name == key {
			value = v
		}
	}
	return value
}

// coopCommand is coop with args, in the workspace and in a session of its own, so a stop reaches its
// whole tree. Every box the launch starts carries the sample's label: COOP_RUN_ARGS is the person's
// own plus the label, and an editor session's per-provider coop processes inherit it too.
func (b *bench) coopCommand(token string, env []string, args ...string) *exec.Cmd {
	runArgs := "--label " + sampleLabel + "=" + token
	if own := b.getenv("COOP_RUN_ARGS"); own != "" {
		runArgs = own + " " + runArgs
	}
	cmd := exec.Command(b.coop, args...)
	cmd.Dir = b.repo
	cmd.Env = append(slices.Concat(b.env, env), "COOP_RUN_ARGS="+runArgs)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// process is one coop the bench launched.
type process struct {
	cmd            *exec.Cmd
	stdout, stderr output
	done           chan struct{} // closed once it has exited and its output has ended
}

// output is a launch's stream as it arrives; a stop reads it while coop may still be writing.
type output struct {
	mu   sync.Mutex
	data []byte
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data = append(o.data, p...)
	return len(p), nil
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.data)
}

// begin starts cmd, keeping what it prints wherever the caller did not take it, and claims what the
// launch makes for ours.
func (b *bench) begin(cmd *exec.Cmd, ours *ownership) (*process, error) {
	p := &process{cmd: cmd, done: make(chan struct{})}
	if cmd.Stdout == nil {
		cmd.Stdout = &p.stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = &p.stderr
	}
	if err := b.start(cmd); err != nil {
		return nil, err
	}
	ours.pid = cmd.Process.Pid
	go func() {
		_ = cmd.Wait() // its exit status is read from cmd.ProcessState
		close(p.done)
	}()
	return p, nil
}

// wait returns once the launch has ended, errTimeout once timeout passes, or errInterrupted on a
// person's Ctrl-C.
func (p *process) wait(ctx context.Context, timeout time.Duration) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
		return errTimeout
	case <-ctx.Done():
		return errInterrupted
	}
}

// ended reports whether the launch ends within d.
func (p *process) ended(d time.Duration) bool {
	return p.wait(context.Background(), d) == nil
}

// stop ends a launch with stopSteps and returns everything it printed: the line naming its network
// run may come last, and without it the run's own gateway would look like another run's.
func (b *bench) stop(p *process) string {
	for _, step := range b.stopSteps {
		if p.ended(0) || syscall.Kill(-p.cmd.Process.Pid, step.signal) != nil || p.ended(step.grace) {
			break
		}
	}
	p.ended(time.Second) // a group that is already gone may still be handing over its last output
	return p.stdout.String() + p.stderr.String()
}

// finish stops a launch whatever its case decided, hears what it printed, and takes back what it
// made: a launch that outlived its case, or a bench interrupted mid-launch, is stopped and cleaned too.
func (b *bench) finish(p *process, ours *ownership, before set) {
	ours.heard(b.stop(p))
	b.clearMarker()
	b.cleanUp(before, ours)
}

// launchRun starts `coop run <extra> -- <command>` the way a script does.
func (b *bench) launchRun(ours *ownership, extra, env []string, command ...string) (*process, error) {
	cmd := b.coopCommand(ours.token, env, slices.Concat([]string{"run"}, extra, []string{"--"}, command)...)
	cmd.Stdin = os.Stdin
	return b.begin(cmd, ours)
}

func (b *bench) marker() string { return filepath.Join(b.repo, readyFile) }

func (b *bench) clearMarker() { _ = os.Remove(b.marker()) }

// awaitMarker returns the host instant the box announced itself; false if the launch ended first or
// the case's time ran out.
func (b *bench) awaitMarker(ctx context.Context, p *process) (time.Time, bool, error) {
	for deadline := time.Now().Add(b.caseTimeout); ; {
		// Whether the launch ended is asked before the marker is: one that wrote it and left is timed.
		ended := p.ended(0)
		if exists(b.marker()) {
			return time.Now(), true, nil
		}
		if ended || !time.Now().Before(deadline) {
			return time.Time{}, false, nil
		}
		if err := sleep(ctx, 5*time.Millisecond); err != nil {
			return time.Time{}, false, err
		}
	}
}

// caseStart times from "the person pressed enter" to "their command was running in the box".
func (b *bench) caseStart(ctx context.Context, extra []string) (sample, error) {
	return b.sampleRun(ctx, extra, false)
}

// caseStop times from the in-box work announcing itself to nothing this run owned remaining.
//
// The clock stops when the runtime lists no container OR VOLUME this run created and coop has
// exited. Returning early is exactly the failure this measurement exists to catch.
func (b *bench) caseStop(ctx context.Context, extra []string) (sample, error) {
	return b.sampleRun(ctx, extra, true)
}

// sampleRun is one launch, measured at its start or its stop, and cleaned up whatever happens.
func (b *bench) sampleRun(ctx context.Context, extra []string, atStop bool) (sample, error) {
	b.clearMarker()
	before, err := b.owned()
	if err != nil {
		return sample{}, err
	}
	ours := b.newOwnership()
	launched := time.Now()
	p, err := b.launchRun(ours, extra, nil, "sh", "-c", readyProbe)
	if err != nil {
		return sample{}, err
	}
	defer b.finish(p, ours, before)
	ready, announced, err := b.awaitMarker(ctx, p)
	if err != nil {
		return sample{}, err
	}
	if err := p.wait(ctx, b.caseTimeout); err != nil {
		return sample{}, err
	}
	ours.heard(p.stdout.String() + p.stderr.String())
	if !announced {
		said := p.stderr.String()
		if said == "" {
			said = p.stdout.String()
		}
		if detail := redact(tail(said, 400)); detail != "" {
			return failed(detail), nil
		}
		return failed("the box never announced itself"), nil
	}
	returned := time.Now()
	gone, err := b.waitUntilGone(ctx, before, ours)
	if err != nil {
		return sample{}, err
	}
	if !atStop {
		if !gone {
			return failed("a finished run left a resource behind"), nil
		}
		return measured(ready.Sub(launched)), nil
	}
	if !gone {
		return failed(fmt.Sprintf("an owned resource outlived the command by %gs", b.goneTimeout.Seconds())), nil
	}
	at := time.Now()
	return measured(at.Sub(ready), fact{"after_process_returned_s", round(at.Sub(returned).Seconds(), 4)},
		fact{"probe_resolution_s", probeResolutionNote}), nil
}

// tail is the last n bytes of text, from a whole character.
func tail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	text = text[len(text)-n:]
	return strings.ToValidUTF8(text, "")
}

// caseFailedStartPreflight times a launch refused before anything is created. Cheap by construction
// — the interesting number is how fast a person learns, not how much was cleaned up, because nothing
// was.
func (b *bench) caseFailedStartPreflight(ctx context.Context, extra []string) (sample, error) {
	before, err := b.owned()
	if err != nil {
		return sample{}, err
	}
	ours := b.newOwnership()
	started := time.Now()
	p, err := b.launchRun(ours, extra, []string{"COOP_IMAGE=coop-bench-no-such-image:absent"}, "sh", "-c", "true")
	if err != nil {
		return sample{}, err
	}
	defer b.finish(p, ours, before)
	if err := p.wait(ctx, b.caseTimeout); err != nil {
		return sample{}, err
	}
	ours.heard(p.stdout.String() + p.stderr.String())
	elapsed := time.Since(started)
	if p.cmd.ProcessState.ExitCode() == 0 {
		return failed("a missing image did not fail the launch"), nil
	}
	if left, err := b.left(before, ours); err != nil {
		return sample{}, err
	} else if len(left) > 0 {
		return failed("a refused launch created a resource"), nil
	}
	return measured(elapsed), nil
}

// caseFailedStartAfterCreate times a launch that fails AFTER the box exists: the box is built,
// mounted and started, and the command inside it does not exist. This is where cleanup work actually
// lives, which the preflight refusal never touches.
func (b *bench) caseFailedStartAfterCreate(ctx context.Context, extra []string) (sample, error) {
	before, err := b.owned()
	if err != nil {
		return sample{}, err
	}
	ours := b.newOwnership()
	started := time.Now()
	p, err := b.launchRun(ours, extra, nil, "/no/such/binary")
	if err != nil {
		return sample{}, err
	}
	defer b.finish(p, ours, before)
	if err := p.wait(ctx, b.caseTimeout); err != nil {
		return sample{}, err
	}
	ours.heard(p.stdout.String() + p.stderr.String())
	elapsed := time.Since(started)
	if p.cmd.ProcessState.ExitCode() == 0 {
		return failed("a missing in-box command did not fail the run"), nil
	}
	if gone, err := b.waitUntilGone(ctx, before, ours); err != nil {
		return sample{}, err
	} else if !gone {
		left, err := b.left(before, ours)
		if err != nil {
			return sample{}, err
		}
		return failed(fmt.Sprintf("a failed run left %d resource(s) behind", len(left))), nil
	}
	return measured(elapsed), nil
}

// caseCancelledStop times Ctrl-C at a box that is doing work: how long until nothing of it is left.
//
// On a REAL terminal, because that is the only place this is measurable. Coop runs the runtime
// client in coop's OWN foreground process group precisely so the terminal delivers ^C to the client
// — and through it to the container. Signalling a process group from a script instead reaches coop
// and not the workload: measured that way this case reported that cancellation never happened at
// all, while a person pressing ^C gets an exit in a fraction of a second.
func (b *bench) caseCancelledStop(ctx context.Context, extra []string) (sample, error) {
	b.clearMarker()
	before, err := b.owned()
	if err != nil {
		return sample{}, err
	}
	ours := b.newOwnership()
	master, tty, err := openPTY()
	if err != nil {
		return sample{}, err
	}
	term := &terminal{fd: master}
	cmd := b.coopCommand(ours.token, nil, slices.Concat([]string{"run"}, extra, []string{"--", "sh", "-c", readyProbe + "; sleep 120"})...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr.Setctty = true // the terminal is coop's controlling one (its stdin), as a person's is
	p, err := b.begin(cmd, ours)
	tty.Close() // coop holds its own copy
	if err != nil {
		term.close()
		return sample{}, err
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		p.ended(killGrace)
		term.close() // hangs up the session; leaving it open leaked a descriptor per sample
		b.clearMarker()
		ours.heard(string(term.said))
		b.cleanUp(before, ours)
	}()
	for deadline := time.Now().Add(b.caseTimeout); ; {
		ended := p.ended(0) // asked first, as in awaitMarker
		if exists(b.marker()) {
			break
		}
		if ended || !time.Now().Before(deadline) {
			return failed("the box never announced itself"), nil
		}
		if ctx.Err() != nil {
			return sample{}, errInterrupted
		}
		term.drain(10 * time.Millisecond)
	}
	signalled, err := term.interrupt()
	if err != nil {
		return sample{}, err
	}
	for !p.ended(0) {
		if time.Since(signalled) >= cancelTimeout {
			return failed(fmt.Sprintf("coop did not exit within %gs of ^C", cancelTimeout.Seconds())), nil
		}
		if ctx.Err() != nil {
			return sample{}, errInterrupted
		}
		term.drain(10 * time.Millisecond)
	}
	// its last lines name its network run: without them a gateway left behind is not its own
	for term.drain(50 * time.Millisecond) {
	}
	ours.heard(string(term.said))
	if gone, err := b.waitUntilGone(ctx, before, ours); err != nil {
		return sample{}, err
	} else if !gone {
		return failed("a cancelled run left a resource behind"), nil
	}
	return measured(time.Since(signalled)), nil
}

// terminal is the bench's side of the pseudo-terminal coop runs on: it types for the person and
// keeps what coop printed.
type terminal struct {
	fd     int
	said   []byte
	closed bool // the far side has gone (EOF or EIO): nothing more will come
}

// drain reads what coop printed, waiting up to timeout for something to arrive; false when nothing
// did.
func (t *terminal) drain(timeout time.Duration) bool {
	if t.closed {
		time.Sleep(timeout)
		return false
	}
	for {
		var readable unix.FdSet
		readable.Set(t.fd)
		wait := unix.NsecToTimeval(timeout.Nanoseconds())
		n, err := unix.Select(t.fd+1, &readable, nil, nil, &wait)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			return false
		}
		break
	}
	chunk := make([]byte, 4096)
	n, err := unix.Read(t.fd, chunk)
	for err == unix.EINTR {
		n, err = unix.Read(t.fd, chunk)
	}
	if n <= 0 {
		t.closed = true
		return false
	}
	t.said = append(t.said, chunk[:n]...)
	return true
}

// interrupt types Ctrl-C, exactly as a person does, and returns the instant it did. Ctrl-C flushes
// the terminal's unread output, which may carry the only network identity, so what is already
// there is read first.
func (t *terminal) interrupt() (time.Time, error) {
	for t.drain(0) {
	}
	at := time.Now()
	_, err := unix.Write(t.fd, []byte{0x03})
	return at, err
}

func (t *terminal) close() { _ = unix.Close(t.fd) }
