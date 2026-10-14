package procharness

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestStdioRetainsFinalFrameAfterReaping(t *testing.T) {
	const frames = "ready\n{\"final\":true}"
	for _, exit := range []string{"0", "7"} {
		t.Run(exit, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", "printf 'ready\\n{\"final\":true}'; exit "+exit)
			stdin, stdout, err := StartStdio(cmd)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { stdin.Close(); stdout.Close(); _ = cmd.Process.Kill() })
			err = cmd.Wait() // reap before any read, including a nonzero exit
			var status *exec.ExitError
			if exit == "0" && err != nil || exit == "7" && (!errors.As(err, &status) || status.ExitCode() != 7) {
				t.Fatalf("wrong exit result: %v", err)
			}
			data, err := io.ReadAll(stdout)
			if err != nil || string(data) != frames {
				t.Fatalf("final unterminated frame lost after Wait: %q, %v", data, err)
			}
		})
	}
}

func TestStdioStartFailureReturnsNoLiveEnds(t *testing.T) {
	stdin, stdout, err := StartStdio(exec.Command("coop-no-such-stdio-executable"))
	if err == nil || stdin != nil || stdout != nil {
		t.Fatalf("failed start exposed live ends: %v, %v, %v", stdin, stdout, err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.Stdout = io.Discard
	if stdin, stdout, err := StartStdio(cmd); err == nil || stdin != nil || stdout != nil {
		t.Fatalf("existing stdout was overwritten: %v, %v, %v", stdin, stdout, err)
	}
}

func TestStdioSetupFailureReleasesDescriptors(t *testing.T) {
	const child = "COOP_TEST_STDIO_FD_EXHAUSTION"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), wait.Deadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStdioSetupFailureReleasesDescriptors$")
		cmd.Env = append(os.Environ(), child+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated descriptor check: %v\n%s", err, output)
		}
		return
	}
	r, w, err := os.Pipe() // initialize the poller before reserving descriptor slots
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
	stdin, stdout, err := StartStdio(exec.Command("/bin/sh", "-c", "exit 0"))
	if !errors.Is(err, syscall.EMFILE) || stdin != nil || stdout != nil {
		t.Fatalf("setup must refuse with no owned ends: %v, %v, %v", stdin, stdout, err)
	}
	r, w, err = os.Pipe()
	if err != nil {
		t.Fatalf("setup leaked the first pipe: %v", err)
	}
	r.Close()
	w.Close()
	// StdinPipe's earlier refusal follows the same close path without exhausting the process.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.Stdin = strings.NewReader("")
	if stdin, stdout, err := StartStdio(cmd); err == nil || stdin != nil || stdout != nil {
		t.Fatalf("configured stdin should refuse: %v, %v, %v", stdin, stdout, err)
	}
}
