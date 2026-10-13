package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestHelperKeepsStdoutAfterProcessExit(t *testing.T) {
	for _, exit := range []string{"0", "7"} {
		t.Run(exit, func(t *testing.T) {
			helper, err := (Runtime{Name: "sh"}).StartHelper(io.Discard, "-c", "printf 'ready\\nfinal\\n'; exit "+exit)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := helper.Close(); err != nil {
					t.Errorf("close: %v", err)
				}
				_ = helper.Stdout.Close()
			})
			// Observe reaping before reading: Wait must not discard the buffered output.
			select {
			case <-helper.Exited():
			case <-time.After(wait.Deadline):
				t.Fatal("helper did not exit")
			}
			data, err := io.ReadAll(helper.Stdout)
			if err != nil || string(data) != "ready\nfinal\n" {
				t.Fatalf("stdout after exit = %q, %v; want complete output and EOF", data, err)
			}
		})
	}
}

func TestHelperFailsClosedWhenProcessCannotStart(t *testing.T) {
	helper, err := (Runtime{Name: "coop-no-such-helper-executable"}).StartHelper(io.Discard)
	if helper != nil || err == nil {
		t.Fatalf("missing helper = %v, %v; want nil and start failure", helper, err)
	}
}

func TestHelperSetupFailureReleasesDescriptors(t *testing.T) {
	const child = "COOP_TEST_HELPER_FD_EXHAUSTION"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperSetupFailureReleasesDescriptors$")
		cmd.Env = append(os.Environ(), child+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated descriptor check: %v\n%s", err, output)
		}
		return
	}
	// Isolate the limit to this child and initialize the pipe poller before filling descriptors.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	_ = w.Close()
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
			_ = file.Close()
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
		t.Fatal("could not reserve two descriptor slots")
	}
	_ = files[len(files)-1].Close()
	_ = files[len(files)-2].Close()
	files = files[:len(files)-2]
	// One pipe can open, but the next must fail; both ends of the first must be released.
	helper, err := (Runtime{Name: "/bin/sh"}).StartHelper(io.Discard, "-c", "exit 0")
	if helper != nil || !errors.Is(err, syscall.EMFILE) {
		t.Fatalf("exhausted setup = %v, %v; want nil and descriptor refusal", helper, err)
	}
	r, w, err = os.Pipe()
	if err != nil {
		t.Fatalf("failed helper setup leaked descriptors: %v", err)
	}
	_ = r.Close()
	_ = w.Close()
}
