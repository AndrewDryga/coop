package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// debugShell opens an interactive shell in the box against the same repo/image as the
// loop iteration, so --debug-on-fail can inspect the failed state. The box is disposable
// per iteration, so this is a fresh shell in the same context, not the failed container.
func (c *Control) debugShell(repo, img, agent string) {
	_, _ = box.Run(c.cfg, c.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: []string{c.cfg.Shell}, Agent: agent,
		Homes: c.cfg.Homes, Network: c.cfg.Network, Cache: c.cfg.Cache,
	})
}

const progressPoll = 2 * time.Second // how often the live bar re-reads the queue while an iteration runs

// IterationCommand adds streaming flags only to coop's known headless forms on a TTY. Claude's
// existing form appends them after the prompt; the other CLIs require their trailing prompt token
// (or -p/value pair) to remain last.
// IterationCommand always requests the adapter's structured stream for a built-in command —
// on a TTY for the live view, and on redirected runs (fork logs, CI) because the stream is the
// provider watchdog's only trustworthy activity signal. Custom work commands keep their own
// output: Coop does not own their protocol, so they run unstreamed and unwatched.
func IterationCommand(agent string, cmd, custom []string) ([]string, bool) {
	if len(custom) > 0 {
		return custom, false
	}
	adapter, ok := agents.Get(agent)
	if !ok {
		return cmd, false
	}
	stream := adapter.Stream()
	if stream.Format == agents.StreamNone || len(stream.Flags) == 0 {
		return cmd, false
	}
	return spliceBeforeTrailing(cmd, stream.Flags, stream.TrailingArgs), true
}

func spliceBeforeTrailing(cmd, insert []string, trailing int) []string {
	if len(insert) == 0 {
		return cmd
	}
	at := len(cmd) - trailing
	if at < 0 {
		at = 0
	}
	result := make([]string, 0, len(cmd)+len(insert))
	result = append(result, cmd[:at]...)
	result = append(result, insert...)
	return append(result, cmd[at:]...)
}

const maxClaudePlainLimitBytes = 512

// claudePlainLimitProbe keeps only small, complete non-streaming stdout. Claude can print its
// model-credit denial there and exit nonzero, but ordinary assistant prose also uses stdout, so a
// truncated tail is unsafe: any overflow or extra text must invalidate the signal.
type claudePlainLimitProbe struct {
	mu       sync.Mutex
	buf      []byte
	overflow bool
}

func (p *claudePlainLimitProbe) Write(chunk []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.overflow || len(chunk) > maxClaudePlainLimitBytes-len(p.buf) {
		p.buf = nil
		p.overflow = true
		return len(chunk), nil
	}
	p.buf = append(p.buf, chunk...)
	return len(chunk), nil
}

func (p *claudePlainLimitProbe) limited(code int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if code == 0 || p.overflow {
		return false
	}
	return claudeCreditLimitNotice(strings.TrimSpace(string(p.buf)))
}

// runIteration runs one boxed command in batch mode, teeing its output to the terminal while
// capturing a response tail and a separate terminal-diagnostic tail. hosts are the queue files the
// live bar watches task counts while its explicit activity remains fixed. On interactive terminals
// the agent's output is funneled into the scroll history above a sticky progress bar (a
// Docker-build-style live view). Non-terminal output goes straight to the destination unchanged.
func (c *Control) runIteration(ctx context.Context, repo, img, agent, forkName string, cmd []string, streaming bool, hosts []string, windowMode completionWindowMode, reviewSubjects []string, repoReadOnly bool, sink io.Writer, peers []agents.Target, activity, assignedTask string) (code int, output string, res *iterResult, classification iterationClassification, windows *tasks.CompletionWindowSet, err error) {
	if windowMode == completionWindowReview {
		windows, err = tasks.BeginReviewCompletionWindows(hosts, reviewSubjects)
	} else if windowMode == completionWindowWork {
		if len(reviewSubjects) != 1 {
			err = errors.New("work completion window requires one assigned subject")
		} else {
			windows, err = tasks.BeginWorkCompletionWindows(hosts, reviewSubjects[0])
		}
	} else {
		windows, err = tasks.BeginCompletionWindows(hosts)
	}
	if err != nil {
		err = fmt.Errorf("%w: %v", tasks.ErrCompletionWindowSetup, err)
		classification = classifyIteration(agent, 1, err, err.Error(), streamNotUsed, time.Now())
		return 1, "", nil, classification, nil, err
	}
	tail := &tailWriter{max: 64 << 10}
	diagnostic := &tailWriter{max: 64 << 10}
	live := loopBarSupported(os.Getenv("TERM_PROGRAM"), ui.IsTerminal(os.Stdout), ui.IsTerminal(os.Stderr))

	termOut, termErr := io.Writer(os.Stdout), io.Writer(os.Stderr)
	var bar *loopBar
	var funnel *lineWriter
	var liveWidth func() int
	if live {
		liveWidth = func() int { return ui.TermWidth(os.Stderr) }
		region := ui.NewRegion(os.Stderr, liveWidth)
		c0, _ := tasks.QueueProgress(hosts)
		bar = newLoopBar(region, liveWidth, time.Now(), c0, activity)
		funnel = &lineWriter{fn: bar.history} // agent/loop lines scroll above the bar
		termOut, termErr = funnel, funnel
		// Route coop's own status lines (ui.Info etc. — from here AND box.Run's startup: "shadowed",
		// "starting sibling services") through the bar too, so they scroll above it instead of
		// overprinting it. Deferred clear restores plain stderr once the iteration's bar is gone.
		ui.SetLiveSink(bar.history)
		defer ui.SetLiveSink(nil)
	}

	outWs := []io.Writer{termOut}
	errWs := []io.Writer{termErr, tail, diagnostic}
	if sink != nil { // fork loops also capture to ../<repo>-forks/.coop/<name>.log
		outWs = append(outWs, sink)
		errWs = append(errWs, sink)
	}
	// A built-in loop command on a TTY emits its provider's streaming JSON. Decode it into human
	// activity lines, keeping narration out of the terminal diagnostics used for recovery policy.
	rawTrace, renderedTrace, closeTrace := c.iterationStreamTrace(repo, agent, streaming)
	defer closeTrace()
	if renderedTrace != nil {
		outWs = append(outWs, renderedTrace)
	}
	var stdoutW io.Writer
	var dec iterationStreamDecoder
	var plainClaudeLimit *claudePlainLimitProbe
	if streaming {
		dec = newIterationStreamDecoder(agent, io.MultiWriter(outWs...), tail, diagnostic, c.cfg.ActiveProfile(agent), box.Workdir(c.cfg, repo), c.cfg.ModelFor(agent))
	}
	if dec != nil {
		dec.setDisplayWidth(liveWidth)
		stdoutW = dec
		if rawTrace != nil {
			stdoutW = io.MultiWriter(rawTrace, dec)
		}
	} else {
		// Plain stdout mixes assistant prose with provider output. It is useful response context,
		// but only stderr can safely steer retries when no structured decoder separates the two.
		plainWs := append(outWs, tail)
		if rawTrace != nil {
			plainWs = append([]io.Writer{rawTrace}, plainWs...)
		}
		if agent == "claude" && !streaming {
			plainClaudeLimit = &claudePlainLimitProbe{}
			plainWs = append(plainWs, plainClaudeLimit)
		}
		stdoutW = io.MultiWriter(plainWs...)
	}
	var stderrW io.Writer = io.MultiWriter(errWs...)
	var stderrFilter *stderrLineFilter
	switch dec.(type) {
	case *codexStreamDecoder:
		stderrFilter = newCodexStderrFilter(stderrW)
	case *geminiStreamDecoder:
		stderrFilter = newGeminiStderrFilter(stderrW)
	}
	if stderrFilter != nil {
		stderrW = stderrFilter
	}

	var wg sync.WaitGroup
	var stop chan struct{}
	if live {
		stop = make(chan struct{})
		wg.Add(1)
		go func() { defer wg.Done(); monitorProgress(hosts, stop, bar) }()
		if ui.SpinnerEnabled() {
			wg.Add(1)
			go func() { defer wg.Done(); spinLoop(bar, stop) }()
		}
	}
	// Named --peer peers make each iteration a consult lead: box.Run then mounts exactly
	// those peers' credentials, the coop-consult wrapper, and the second-opinion directive. A
	// preset does the same with ITS roles: the routing contract mounts via ConsultLead.
	lead := ""
	if len(peers) > 0 || c.preset != nil {
		lead = agent
	}
	// A structured stream gives the watchdog trustworthy activity, so only then does the
	// attempt get a child context it may cancel on proven silence. The parent ctx stays
	// untouched: a user interrupt keeps winning over any watchdog fire. box.Run arms the
	// watchdog at its runtime-launch boundary, so the host setup it does first — projection,
	// services, network — is never clocked as a silent provider.
	boxCtx := ctx
	var watchdog *providerWatchdog
	var armWatchdog func()
	if dec != nil {
		parent := ctx
		if parent == nil {
			parent = context.Background()
		}
		childCtx, cancelChild := context.WithCancel(parent)
		defer cancelChild()
		watchdog = newProviderWatchdog(watchdogPolicyFor(c.cfg, agent), cancelChild)
		dec.setActivity(watchdog)
		armWatchdog = watchdog.armStart
		boxCtx = childCtx
	}
	code, err = box.Run(c.cfg, c.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: agent, Batch: true, ForkName: forkName, ForkOwner: c.forkOwner, ConsultLead: lead, Peers: peers, Preset: c.preset, RunID: c.runID, AssignedTask: assignedTask,
		SuperviseDescendants: true,
		RepoReadOnly:         repoReadOnly,
		RepoReadOnlyPaths:    reviewReadOnlyPaths(windowMode, repoReadOnly, hosts),
		Homes:                c.cfg.Homes, Network: c.cfg.Network, Cache: c.cfg.Cache, Serve: true,
		Stdout:          stdoutW,
		Stderr:          stderrW,
		Ctx:             boxCtx,
		OnRuntimeLaunch: armWatchdog,
	})
	if watchdog != nil {
		watchdog.stop() // nothing may fire once the box run returned
	}
	if live {
		close(stop)
		wg.Wait() // no goroutine repaints the region after this, so the teardown below is clean
	}
	streamOutcome := streamNotUsed
	if dec != nil {
		dec.flush()                // before tail.String(): final events must reach the rate-limit tail
		res = dec.lastIterResult() // result cost/turns/tokens (nil if none landed), for telemetry
		streamOutcome = dec.streamOutcome()
		if claude, ok := dec.(*streamDecoder); ok {
			claude.promoteTerminalLimitDiagnostic(code, streamOutcome)
		}
		if streamErr := validateProviderStream(code, err, streamOutcome); err == nil && streamErr != nil {
			message := streamErr.Error()
			fmt.Fprintln(termErr, message)
			_, _ = io.WriteString(tail, message+"\n")
			_, _ = io.WriteString(diagnostic, message+"\n")
			err = streamErr
		}
	}
	if stderrFilter != nil {
		if flushErr := stderrFilter.flush(); err == nil {
			err = flushErr
		}
	}
	if plainClaudeLimit != nil {
		if plainClaudeLimit.limited(code) {
			_, _ = io.WriteString(diagnostic, "rate limit exceeded\n")
		}
	}
	if live {
		funnel.flush()
		bar.stop()
	}
	output = tail.String()
	if windowMode == completionWindowReview && agent == "codex" {
		output, _ = normalizeCodexReviewOutput(output)
	}
	classification = classifyIteration(agent, code, err, diagnostic.String(), streamOutcome, time.Now())
	// A watchdog kill owns the classification only when the attempt actually died and the
	// parent was not interrupted: parent cancellation wins and remains "interrupted", and a
	// provider that finished before a racing fire keeps its real outcome.
	if watchdog != nil && err != nil && (ctx == nil || ctx.Err() == nil) {
		if timeout := watchdog.timedOut(); timeout != "" {
			classification = iterationClassification{outcome: timeout, detail: watchdog.timeoutDiagnostic()}
		}
	}
	return code, output, res, classification, windows, err
}

// monitorProgress watches the queue while an iteration runs and pushes count changes into the live
// bar. The activity is owned by runIteration and cannot drift to another queue item when a task
// moves; only done/blocked/total counts are monitored.
func monitorProgress(hosts []string, stop <-chan struct{}, bar *loopBar) {
	t := time.NewTicker(progressPoll)
	defer t.Stop()
	last, _ := tasks.QueueProgress(hosts) // the bar was built with this baseline
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			last = updateLoopBarCounts(hosts, last, bar)
		}
	}
}

func updateLoopBarCounts(hosts []string, last tasks.TaskCounts, bar *loopBar) tasks.TaskCounts {
	// c.total()==0 while we had a baseline is a torn read (a folder caught mid-move) — a
	// running loop always has tasks; keep the last good counts rather than blink to 0/0.
	c, _ := tasks.QueueProgress(hosts)
	if c != last && (c.Total() > 0 || last.Total() == 0) {
		bar.setCounts(c)
		return c
	}
	return last
}

func reviewActivity(stage string, subjects []string) string {
	if len(subjects) == 0 {
		return stage
	}
	prefix := stage + ": "
	suffix := ""
	if len(subjects) > 1 {
		suffix = fmt.Sprintf(" +%d", len(subjects)-1)
	}
	return prefix + subjects[0] + suffix
}

// agentLoopCmd builds the headless, autonomous command for one loop iteration of the
// given agent, carrying prompt (each agent's non-interactive form lives in its adapter).
func (c *Control) agentLoopCmd(agent, prompt string) []string {
	if ag, ok := agents.Get(agent); ok {
		return ag.Headless(c.cfg, prompt)
	}
	return append([]string{agent}, prompt)
}
