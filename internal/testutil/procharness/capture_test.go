package procharness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestCaptureDrainsLargeCombinedOutputAndRestoresStreams(t *testing.T) {
	oldOut, oldErr := os.Stdout, os.Stderr
	body := strings.Repeat("large-output\n", 1<<17)
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, err := CaptureOutput(func() {
			fmt.Fprint(os.Stdout, "first\n")
			fmt.Fprint(os.Stderr, body)
			fmt.Fprint(os.Stdout, "last\n")
		}, &os.Stdout, &os.Stderr)
		if err != nil || out != "first\n"+body+"last\n" {
			t.Errorf("large combined output truncated/reordered: %d bytes, %v", len(out), err)
		}
	}()
	select {
	case <-done:
	case <-time.After(wait.Deadline):
		// Release a broken writer so even the mutation's failure cannot poison later tests.
		if os.Stdout != oldOut {
			os.Stdout.Close()
		}
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Fatal("capture writer remained blocked after close")
		}
		t.Fatal("capture blocked on output above pipe capacity")
	}
	if os.Stdout != oldOut || os.Stderr != oldErr {
		t.Fatal("normal capture did not restore both streams")
	}
}

func TestCaptureRestoresStreamsOnPanicAndGoexit(t *testing.T) {
	for _, exit := range []bool{false, true} {
		oldOut, oldErr := os.Stdout, os.Stderr
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if !exit && recover() != "capture panic" {
					t.Error("callback panic was swallowed or changed")
				}
			}()
			_, _ = CaptureOutput(func() {
				fmt.Fprint(os.Stderr, "before abort")
				if exit {
					runtime.Goexit() // testing.Fatal/Skip uses the same unwinding path
				}
				panic("capture panic")
			}, &os.Stdout, &os.Stderr)
		}()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Fatal("aborted capture did not close/join")
		}
		if os.Stdout != oldOut || os.Stderr != oldErr {
			t.Fatal("aborted callback poisoned process-global output")
		}
		out, err := CaptureOutput(func() { fmt.Fprint(os.Stderr, "next capture") }, &os.Stderr)
		if err != nil || out != "next capture" {
			t.Fatalf("later capture was poisoned: %q, %v", out, err)
		}
	}
}

func TestCaptureReportsCloseFailureAndStillRestoresOutput(t *testing.T) {
	old := os.Stdout
	output, err := CaptureOutput(func() {
		fmt.Fprint(os.Stdout, "before close")
		if err := os.Stdout.Close(); err != nil {
			t.Fatal(err)
		}
	}, &os.Stdout)
	if !errors.Is(err, os.ErrClosed) || output != "before close" || os.Stdout != old {
		t.Fatalf("close error/output/restoration lost: %q, %v, restored=%v", output, err, os.Stdout == old)
	}
}

func TestCaptureClosesBothDescriptorsBeforeReturning(t *testing.T) {
	const child = "COOP_TEST_CAPTURE_FD_EXHAUSTION"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), wait.Deadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCaptureClosesBothDescriptorsBeforeReturning$")
		cmd.Env = append(os.Environ(), child+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated capture descriptor check: %v\n%s", err, output)
		}
		return
	}
	debug.SetGCPercent(-1) // a file finalizer must not disguise a missing explicit close
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	w.Close()
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	limit.Cur = min(limit.Cur, 64)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	var files []*os.File
	defer func() {
		for _, file := range files {
			file.Close()
		}
	}()
	for {
		file, err := os.Open(os.DevNull)
		if errors.Is(err, syscall.EMFILE) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if len(files) < 2 {
		t.Fatal("could not reserve two slots")
	}
	files[len(files)-1].Close()
	files[len(files)-2].Close()
	files = files[:len(files)-2]
	if _, err := CaptureOutput(func() {}, &os.Stderr); err != nil {
		t.Fatal(err)
	}
	r, w, err = os.Pipe()
	if err != nil {
		t.Fatalf("capture leaked a descriptor before returning: %v", err)
	}
	r.Close()
	w.Close()
}
