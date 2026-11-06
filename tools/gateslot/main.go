// Command gateslot runs a command once it holds one of a few gate slots on this host, so only that
// many full gates run at once.
//
// A main loop and a few fork loops each run `make check` in their own box. All of them at once
// pushed a 12-core Mac to a load of 40, the scripted suites hit their timeouts, and audits reopened
// good work. Every box mounts the same coop-cache volume as ~/.cache, so a flock on a file there is
// seen by every box on the host; each slot is one file, held for the whole gate. The kernel drops a
// flock when its holder dies, so a crashed or killed gate frees its slot with no cleanup.
//
// The command starts only after the slot is taken, so the time spent waiting never counts against
// the gate's own timeouts.
//
//	go run ./tools/gateslot DIR COMMAND [ARG...]
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// slots is how many full gates may run at once on one host.
const slots = 2

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: gateslot DIR COMMAND [ARG...]")
		os.Exit(2)
	}
	os.Exit(gate(os.Args[1], slots, time.Second, os.Stderr, os.Args[2:]))
}

// gate waits for one of n slots under dir, then runs argv with the slot held and returns its exit
// status.
func gate(dir string, n int, poll time.Duration, stderr io.Writer, argv []string) int {
	start := time.Now()
	slot, err := acquire(dir, n, poll, func() {
		fmt.Fprintf(stderr, "Waiting for another gate to finish: %d already run on this host (slots in %s)\n", n, dir)
	})
	if err != nil {
		fmt.Fprintf(stderr, "gateslot: %v\n", err)
		return 1
	}
	defer slot.Close()
	if waited := time.Since(start); waited >= poll {
		fmt.Fprintf(stderr, "Got a gate slot after %s\n", waited.Round(time.Second))
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "gateslot: %v\n", err)
		return 1
	}
	// Pass a stop on to the gate, and keep the slot until the gate has really exited.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	go func() {
		for s := range signals {
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	if err != nil {
		fmt.Fprintf(stderr, "gateslot: %v\n", err)
		return 1
	}
	return 0
}

// acquire returns the open file of the first free slot of n under dir, trying again every poll. It
// calls waiting once, when every slot is first found busy.
func acquire(dir string, n int, poll time.Duration, waiting func()) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for told := false; ; time.Sleep(poll) {
		for i := range n {
			file, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("slot-%d", i)), os.O_RDONLY|os.O_CREATE, 0o644)
			if err != nil {
				return nil, err
			}
			err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				return file, nil
			}
			file.Close()
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, err
			}
		}
		if !told {
			told = true
			waiting()
		}
	}
}
