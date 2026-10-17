package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
	"golang.org/x/sys/unix"
)

// standIn is a stand-in coop: it records the COOP_RUN_ARGS it was given, names its network run the
// way a filtered launch does, and behaves just enough like `coop run` and `coop acp` for each case.
// With STAND_IN_HANG its run outlasts any case's limit (two minutes, so a broken test cannot leave it
// behind for longer), and leaves on INT or TERM. STAND_IN_DELAY makes it slow to start, from the
// environment, so no test rewrites the script it runs.
const standIn = `#!/bin/sh
[ -n "$STAND_IN_DELAY" ] && sleep "$STAND_IN_DELAY"
echo $$ > "@LAUNCHED@.pid"
printf '%s' "$COOP_RUN_ARGS" > "@LAUNCHED@.tmp" && mv "@LAUNCHED@.tmp" "@LAUNCHED@"
echo "Full details: coop net inspect 8b --json" >&2
case "$1" in
run)
	while [ $# -gt 0 ] && [ "$1" != -- ]; do shift; done
	shift
	if [ -n "$STAND_IN_HANG" ]; then
		trap 'exit 0' INT TERM
		: > "@LAUNCHED@.ready"
		n=0
		while [ $n -lt 120 ]; do sleep 1 </dev/null >/dev/null 2>&1 & wait $!; n=$((n + 1)); done
		exit 0
	fi
	: > "@LAUNCHED@.ready"
	[ -n "$COOP_IMAGE" ] && exit 1
	exec "$@" ;;
acp)
	: > "@LAUNCHED@.ready"
	IFS= read -r line
	[ -n "$COOP_ACP_TRACE" ] && exit 0
	printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
	exit 0 ;;
esac
`

// standInBench is a bench whose coop is the stand-in and whose runtime is a world where a launch
// makes a box and a gateway volume of its own while another run makes a box and a database, and
// the launch prints the network run of one more volume. It returns the stand-in's launched file.
func standInBench(t *testing.T) (*bench, *fakeRuntime, string) {
	root := t.TempDir()
	launched := filepath.Join(root, "launched")
	fake := &fakeRuntime{
		appear: [2][]resource{{{"our-box", boxFilter}, {"their-box", boxFilter}},
			{{"our-ipc", gatewayFilter}, {"printed-ipc", gatewayFilter}, {"their-db", gatewayFilter}}},
		launched: launched,
		labels: map[string]labels{"our-ipc": {network: "7a"}, "printed-ipc": {network: "8b"},
			"their-box": {"v1:ws:999:t", "9c", "another-sample"}, "their-db": {network: "9c"}},
	}
	// The box carries the sample's label only if the launch really passed it on.
	fake.onAppear = func(runArgs string) {
		sample := ""
		if i := strings.LastIndex(runArgs, sampleLabel+"="); i >= 0 {
			sample = runArgs[i+len(sampleLabel)+1:]
		}
		fake.labels["our-box"] = labels{network: "7a", sample: sample}
	}
	b := testBench(fake)
	b.coop, b.repo = filepath.Join(root, "coop"), filepath.Join(root, "repo")
	b.env = []string{"PATH=" + os.Getenv("PATH"), "XDG_CONFIG_HOME=" + filepath.Join(root, "config")}
	if err := os.Mkdir(b.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, b.coop, strings.ReplaceAll(standIn, "@LAUNCHED@", launched))
	return b, fake, launched
}

// writeExecutable writes a script this process runs while parallel subtests fork children of their
// own. A child forked while the file is open for writing keeps that descriptor until it execs, and
// running the script then fails with "text file busy" on Linux (golang/go#22315). os/exec forks
// while holding syscall.ForkLock, so holding its read side while the file is open keeps every fork
// out of that window.
func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

var ownLaunch = items("our-box", "our-ipc", "printed-ipc")

func TestEveryCaseCleansUpOnlyWhatItsLaunchMade(t *testing.T) {
	// Whatever the case, cleanup removes what its own launch left (its labelled box, and the gateway
	// resources of the network run that box carries or the launch printed) and never another run's,
	// even one that started while the sample ran.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b, fake, _ := standInBench(t)
			if _, err := c.measure(b, context.Background(), c.extra); err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(fake.removed, ownLaunch) {
				t.Errorf("removed %v, want %v", slices.Sorted(maps.Keys(fake.removed)), slices.Sorted(maps.Keys(ownLaunch)))
			}
		})
	}
}

func TestALaunchThatOutlivesItsCaseIsStoppedAndCleaned(t *testing.T) {
	// The launch hangs past the case's limit: it is stopped, and what it printed and made is still
	// taken back, though the case itself fails with the timeout.
	for _, tc := range []struct {
		name    string
		measure func(*bench, context.Context, []string) (sample, error)
		delay   bool
	}{
		{"a refused launch", (*bench).caseFailedStartPreflight, false},
		{"a failed run", (*bench).caseFailedStartAfterCreate, false},
		// A scheduling control: startup alone exceeds the unchanged case timeout.
		{"a failed run that is slow to start", (*bench).caseFailedStartAfterCreate, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, fake, launched := standInBench(t)
			if tc.delay {
				b.env = append(b.env, "STAND_IN_DELAY=0.75")
			}
			b.env = append(b.env, "STAND_IN_HANG=1")
			b.caseTimeout = 500 * time.Millisecond
			// The timeout tests cleanup after creation, not how fast this host starts a shell: the
			// launch returns once the stand-in has published its PID, label and network identity.
			b.start = func(cmd *exec.Cmd) error {
				if err := cmd.Start(); err != nil {
					return err
				}
				for deadline := time.Now().Add(wait.Deadline); !exists(launched + ".ready"); time.Sleep(wait.Poll) {
					if time.Now().After(deadline) {
						_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
						return fmt.Errorf("the stand-in did not publish its readiness within %s", wait.Deadline)
					}
				}
				return nil
			}
			if _, err := tc.measure(b, context.Background(), nil); !errors.Is(err, errTimeout) {
				t.Fatalf("the case ended with %v, not its timeout", err)
			}
			if !maps.Equal(fake.removed, ownLaunch) {
				t.Errorf("removed %v", slices.Sorted(maps.Keys(fake.removed)))
			}
			pid, err := strconv.Atoi(strings.TrimSpace(read(t, launched+".pid")))
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Errorf("the hung launch was left running: %v", err)
			}
		})
	}
}

func TestACancelledRunThatLeavesItsGatewayFails(t *testing.T) {
	// Its box is gone and its gateway volume is named only in what it printed to the terminal: still
	// its own, so the sample fails and the volume is taken back.
	b, fake, _ := standInBench(t)
	fake.appear = [2][]resource{nil, {{"printed-ipc", gatewayFilter}}}
	s, err := b.caseCancelledStop(context.Background(), nil)
	if err != nil || s.OK || s.Detail != "a cancelled run left a resource behind" {
		t.Errorf("sample = %+v, %v", s, err)
	}
	if !maps.Equal(fake.removed, items("printed-ipc")) {
		t.Errorf("removed %v", slices.Sorted(maps.Keys(fake.removed)))
	}
}

func TestInterruptReadsWhatTheTerminalHoldsFirst(t *testing.T) {
	// Ctrl-C flushes a terminal's unread output, which may carry the only network identity a
	// cancelled run prints, so interrupt reads what is already there before it types. macOS flushes
	// what the bench has not read; Linux keeps what reached the bench's side, so there this passes
	// either way.
	master, tty, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	term := &terminal{fd: master}
	t.Cleanup(func() {
		tty.Close()
		term.close()
	})
	if _, err := tty.WriteString("Full details: coop net inspect 6f --json\n"); err != nil {
		t.Fatal(err)
	}
	// The identity reaches the bench's side unread, as a box's output has by the time it is cancelled.
	var readable unix.FdSet
	readable.Set(master)
	timeout := unix.NsecToTimeval(wait.Deadline.Nanoseconds())
	if n, err := unix.Select(master+1, &readable, nil, nil, &timeout); n != 1 || err != nil {
		t.Fatalf("the terminal never had output to read: %v", err)
	}
	if _, err := term.interrupt(); err != nil {
		t.Fatal(err)
	}
	// The terminal echoes ^C once it has handled it, its flush included.
	wait.For(t, "the terminal echoing ^C", func() bool {
		term.drain(10 * time.Millisecond)
		return bytes.Contains(term.said, []byte("^C"))
	})
	if got := networkRuns(string(term.said)); !slices.Equal(got, []string{"6f"}) {
		t.Errorf("the identity printed before Ctrl-C was lost: %q", term.said)
	}
}

func TestAStopEscalatesAndHandsBackWhatItPrinted(t *testing.T) {
	// INT is acknowledged but does not stop the child; TERM prints its final network identity. INT's
	// grace is short here: the 20 s a person's coop gets would be most of this package's time.
	b := &bench{start: (*exec.Cmd).Start, stopSteps: []stopStep{{syscall.SIGINT, 200 * time.Millisecond},
		{syscall.SIGTERM, wait.Deadline}, {syscall.SIGKILL, wait.Deadline}}}
	cmd := exec.Command("sh", "-c", `trap 'echo "ACK INT"' INT
trap 'printf "ACK TERM\ncoop net inspect 6f\n"; exit 0' TERM
echo READY
while :; do sleep 1 </dev/null >/dev/null 2>&1 & wait $!; done`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	p, err := b.begin(cmd, &ownership{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !p.ended(0) { // not yet reaped, so its group id is still its own
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	wait.For(t, "the child installing its traps", func() bool { return strings.Contains(p.stdout.String(), "READY\n") })
	if p.ended(300 * time.Millisecond) {
		t.Fatal("the child ended before any stop")
	}
	said := b.stop(p)
	if !strings.Contains(said, "ACK INT\nACK TERM\n") || !slices.Equal(networkRuns(said), []string{"6f"}) {
		t.Errorf("the stop handed back %q", said)
	}
}

func TestTheSampleLabelRidesOnThePersonsRunArgs(t *testing.T) {
	for own, want := range map[string]string{
		"-v /cache:/cache": "-v /cache:/cache --label coop-bench.sample=tok",
		"":                 "--label coop-bench.sample=tok",
	} {
		b := &bench{coop: "coop", env: []string{"COOP_RUN_ARGS=" + own}}
		cmd := b.coopCommand("tok", nil, "run")
		if got := (&bench{env: cmd.Env}).getenv("COOP_RUN_ARGS"); got != want {
			t.Errorf("COOP_RUN_ARGS %q became %q, want %q", own, got, want)
		}
	}
}
