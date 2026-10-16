// Command servicesbench measures how long a warm box start takes in a project with Compose services
// (Postgres, Redis).
//
// The boundary is the one tools/lifecyclebench uses for its start cases: from the launch to the box's
// own command running. The box writes a marker file into the mounted project and the host watches for
// it, so the instant is the host's own. "Warm" means the images are already pulled and the project's
// volumes exist.
//
// It never removes anything itself: before each sample it runs `coop down` (volumes kept) or `coop up`
// in its own project, so it is safe while other coop runs use the same Docker. Measure on an idle
// machine anyway: other runs make every start slower.
//
// Two cases, taken in turns so both see the same load, each after one throwaway sample:
//
//   - stopped: the services are stopped (`coop down`); the launch starts them
//   - running: the services are already up (`coop up`), as after a first launch
//
// Usage (make runs the bench's own tests first):
//
//	make services-bench WORKSPACE=<project with approved services> OUT=<dir> BENCH_ARGS='--samples 7'
//
// Output: <dir>/samples.json (every sample, with the load average when it started) and a summary on
// stdout: per case the count, failures, median, fastest and slowest.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	mark      = ".coop-services-ready"
	timeout   = 300 * time.Second
	stopGrace = 10 * time.Second // how long a launch's session gets to leave after each signal
)

// errInterrupted is a person's Ctrl-C.
var errInterrupted = errors.New("interrupted")

// bench is one measuring run. Its bounds are fields, so a test can shorten one without the others.
type bench struct {
	coop, repo string
	env        []string      // every coop call's
	timeout    time.Duration // a step, and a launch until its marker
	linger     time.Duration // a launch after that, to end by itself before it is stopped
	stopGrace  time.Duration
}

func newBench() *bench {
	return &bench{timeout: timeout, linger: timeout, stopGrace: stopGrace}
}

// row is one launch: the seconds until the box's command wrote its marker (none if it never did),
// how the launch exited (minus the signal that ended it), the load average when it started, and the
// end of what it said if it failed.
type row struct {
	Seconds *float64 `json:"seconds"`
	Exit    int      `json:"exit"`
	Load    float64  `json:"load"`
	Tail    string   `json:"tail"`
}

type summary struct {
	N      int      `json:"n"`
	Failed int      `json:"failed"`
	P50    *float64 `json:"p50"`
	Min    *float64 `json:"min"`
	Max    *float64 `json:"max"`
}

// perCase holds one value for each case, written in the order the cases are taken.
type perCase[T any] struct {
	Stopped T `json:"stopped"`
	Running T `json:"running"`
}

func main() {
	// Ctrl-C stops the launch in flight and still takes the services down; a second Ctrl-C ends the
	// bench at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	context.AfterFunc(ctx, stop)
	code := run(ctx, os.Args[1:], newBench(), os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the whole command: b brings its bounds and the flags choose the rest.
func run(ctx context.Context, args []string, b *bench, stdout, stderr io.Writer) int {
	defaultCoop := "coop"
	if found, err := exec.LookPath("coop"); err == nil {
		defaultCoop = found
	}
	flags := flag.NewFlagSet("servicesbench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", "", "a project whose Compose services are approved (required)")
	samples := flags.Int("samples", 7, "samples per case")
	out := flags.String("out", "", "directory for samples.json (required)")
	coop := flags.String("coop", defaultCoop, "the coop binary to measure")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return 0
	} else if err != nil {
		return 2
	}
	if *repo == "" || *out == "" || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: go run ./tools/servicesbench --repo <project> --out <dir> [--samples N] [--coop <path>]")
		return 2
	}
	// Absolute, always: every call runs in the project, so a relative name such as ./coop would
	// resolve against the project there.
	b.coop, b.repo = absolute(*coop), *repo
	project, _ := filepath.Abs(*repo)
	// Every coop call works on the project the bench was given, whatever COOP_REPO the shell has, and
	// lets the launch start the services, whatever the project says: that start is what it times.
	b.env = append(os.Environ(), "COOP_REPO="+project, "COOP_AUTO_UP=1")

	taken, err := b.measure(ctx, *samples)
	_ = b.step(context.WithoutCancel(ctx), "down") // leave the services stopped, however it ended
	if err != nil {
		fmt.Fprintf(stderr, "servicesbench: %v\n", err)
		return 1
	}
	summaries := perCase[summary]{summarize(taken.Stopped), summarize(taken.Running)}
	data, err := json.MarshalIndent(struct {
		Repo    string           `json:"repo"`
		When    string           `json:"when"`
		Summary perCase[summary] `json:"summary"`
		Samples perCase[[]row]   `json:"samples"`
	}{*repo, time.Now().Format("2006-01-02 15:04 MST"), summaries, taken}, "", "  ")
	if err == nil {
		err = os.MkdirAll(*out, 0o755)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(*out, "samples.json"), append(data, '\n'), 0o644)
	}
	if err != nil {
		fmt.Fprintf(stderr, "servicesbench: %v\n", err)
		return 1
	}
	data, _ = json.MarshalIndent(summaries, "", "  ")
	fmt.Fprintln(stdout, string(data))
	return 0
}

// measure takes one throwaway sample of each case, then samples rounds of both in turns.
func (b *bench) measure(ctx context.Context, samples int) (perCase[[]row], error) {
	taken := perCase[[]row]{Stopped: []row{}, Running: []row{}}
	for pass := range samples + 1 {
		for _, c := range []struct {
			step string // what puts the services in the state the case starts from
			rows *[]row
		}{{"down", &taken.Stopped}, {"up", &taken.Running}} {
			r, err := b.sample(ctx, c.step)
			if err != nil {
				return taken, err
			}
			if pass > 0 {
				*c.rows = append(*c.rows, r)
			}
		}
	}
	return taken, nil
}

// sample puts the services in the state a case starts from, then times a launch.
func (b *bench) sample(ctx context.Context, step string) (row, error) {
	if err := b.step(ctx, step); err != nil {
		return row{}, err
	}
	return b.launch(ctx)
}

// step runs `coop up` or `coop down` in the project. If that fails, every later sample would be
// filed under the wrong case, so the bench stops instead.
func (b *bench) step(ctx context.Context, verb string) error {
	bounded, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, b.coop, verb)
	cmd.Dir, cmd.Env, cmd.Stdin = b.repo, b.env, os.Stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A helper the step started can hold its output open after the step (or its timeout) ended it.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay): // the step itself succeeded
		return nil
	case ctx.Err() != nil:
		return errInterrupted
	case bounded.Err() != nil:
		return fmt.Errorf("coop %s did not finish within %gs", verb, b.timeout.Seconds())
	case !errors.As(err, &exit):
		return err
	}
	said := strings.TrimSpace(stderr.String())
	if said == "" {
		said = strings.TrimSpace(stdout.String())
	}
	return fmt.Errorf("coop %s exited %d: %s", verb, exit.ExitCode(), tail(said, 300))
}

// launch is one box start, timed until the box's command wrote its marker.
func (b *bench) launch(ctx context.Context) (row, error) {
	marker := filepath.Join(b.repo, mark)
	_ = os.Remove(marker)
	load := loadAverage()
	// Its output goes to a file: an unread pipe would fill and stall a chatty launch before its marker.
	spool, err := os.CreateTemp("", "servicesbench-*.log")
	if err != nil {
		return row{}, err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	cmd := exec.Command(b.coop, "run", "--", "sh", "-c", ": > ./"+mark)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = b.repo, b.env, os.Stdin, spool, spool
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // a session of its own, so a stop reaches its whole tree
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return row{}, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait() // its exit status is read from cmd.ProcessState
		close(exited)
	}()
	ready, err := b.await(ctx, marker, started, exited)
	b.stop(cmd.Process.Pid, exited) // whatever happened: a launch that hangs after its marker, Ctrl-C
	_ = os.Remove(marker)
	if err != nil {
		return row{}, err
	}
	r := row{Exit: cmd.ProcessState.ExitCode(), Load: round(load, 1)}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		r.Exit = -int(status.Signal()) // -15 obeyed TERM, -9 did not
	}
	if ready.IsZero() {
		said, _ := os.ReadFile(spool.Name())
		r.Tail = tail(string(said), 300)
	} else {
		seconds := round(ready.Sub(started).Seconds(), 3)
		r.Seconds = &seconds
	}
	return r, nil
}

// await returns the instant the marker appeared, zero if it never did, then gives the launch linger
// to finish.
func (b *bench) await(ctx context.Context, marker string, started time.Time, exited <-chan struct{}) (time.Time, error) {
	var ready time.Time
	for {
		// Whether the launch ended is asked before the marker is: one that wrote it and left is timed.
		ended := closed(exited)
		if _, err := os.Stat(marker); err == nil {
			ready = time.Now()
			break
		}
		if ended || time.Since(started) >= b.timeout {
			break
		}
		select {
		case <-ctx.Done():
			return ready, errInterrupted
		case <-time.After(5 * time.Millisecond):
		}
	}
	timer := time.NewTimer(b.linger)
	defer timer.Stop()
	select {
	case <-exited:
	case <-timer.C:
	case <-ctx.Done():
		return ready, errInterrupted
	}
	return ready, nil
}

// stop ends a launch and everything in its session, escalating while anything is left, and reaps it.
// The launch itself leaving is not enough: a child of it that ignores TERM would outlive it.
func (b *bench) stop(group int, exited <-chan struct{}) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if !alive(group) || syscall.Kill(-group, sig) != nil {
			break
		}
		for deadline := time.Now().Add(b.stopGrace); alive(group) && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
		}
	}
	<-exited
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// alive reports whether anything is left of a process group; a process this user may not signal
// counts.
func alive(group int) bool {
	err := syscall.Kill(-group, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// summarize counts a launch that never announced itself as failed, and never times it.
func summarize(rows []row) summary {
	var seconds []float64
	for _, r := range rows {
		if r.Seconds != nil {
			seconds = append(seconds, *r.Seconds)
		}
	}
	s := summary{N: len(seconds), Failed: len(rows) - len(seconds)}
	if len(seconds) > 0 {
		slices.Sort(seconds)
		median := seconds[len(seconds)/2]
		if len(seconds)%2 == 0 {
			median = (seconds[len(seconds)/2-1] + median) / 2
		}
		median = round(median, 2)
		s.P50, s.Min, s.Max = &median, &seconds[0], &seconds[len(seconds)-1]
	}
	return s
}

// loadAverage is the host's one-minute load average, or -1 where it cannot be read.
func loadAverage() float64 {
	text := ""
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		text = string(data)
	} else if data, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil {
		text = strings.Trim(string(data), "{} \n") // macOS says { 3.85 4.12 4.30 }
	}
	if fields := strings.Fields(text); len(fields) > 0 {
		if load, err := strconv.ParseFloat(fields[0], 64); err == nil {
			return load
		}
	}
	return -1
}

// absolute is name as a resolved absolute path, found on PATH first when it is a bare command.
func absolute(name string) string {
	if found, err := exec.LookPath(name); err == nil {
		name = found
	}
	if path, err := filepath.Abs(name); err == nil {
		name = path
	}
	if resolved, err := filepath.EvalSymlinks(name); err == nil {
		name = resolved
	}
	return name
}

// round is x to places decimals, rounded as its decimal form is.
func round(x float64, places int) float64 {
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', places, 64), 64)
	return rounded
}

// tail is the last n bytes of text, from a whole character.
func tail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return strings.ToValidUTF8(text[len(text)-n:], "")
}
