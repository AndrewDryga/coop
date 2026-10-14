package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// Parser canaries intentionally return 1/2. Transport/daemon failure is never a canary result.
func lockedClientProbe(ctx context.Context, docker *runtime.Docker, image, source, home, script string) ([]byte, error) {
	if !strings.HasPrefix(image, "sha256:") || len(image) != 71 || strings.Trim(image[7:], "0123456789abcdef") != "" {
		return nil, fmt.Errorf("native probe requires an immutable image ID")
	}
	var out bytes.Buffer
	bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	code, err := docker.RunWorkloadInterruptible(bounded, nil, &out, &out, "run", "--rm", "--network", "none", "-e", "HOME="+home,
		"-v", source+":/src:ro", "--entrypoint", "sh", image, "-c", script)
	if err != nil {
		return out.Bytes(), err
	}
	if code < 0 || code > 2 {
		return out.Bytes(), fmt.Errorf("native probe transport exited%d", code)
	}
	return out.Bytes(), nil
}

func TestLockedClientProbeInheritsCancellationAndReapsItsChild(t *testing.T) {
	rt := serviceReviewRuntime(t, "")
	ready := filepath.Join(t.TempDir(), "ready")
	script := "#!/bin/sh\necho native-canary\nprintf '%s\\n' $$ > " + strconv.Quote(ready) + "\nexec sleep 30\n"
	if err := os.WriteFile(rt.Name, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	docker, err := runtime.BindDocker(t.Context(), rt, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	source := t.TempDir()
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = lockedClientProbe(ctx, docker, "sha256:"+strings.Repeat("a", 64), source, "/tmp/h", "probe")
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Error("canceled native fixture did not drain")
		}
	})
	pid := 0
	wait.For(t, "native fixture began", func() bool {
		data, err := os.ReadFile(ready)
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		return pid > 0
	})
	cancel()
	select {
	case <-done:
	case <-time.After(wait.Deadline):
		t.Fatal("native probe ignored inherited cancellation")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("native cancellation = %v", runErr)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("native fixture child%d survived: %v", pid, err)
	}
}

func TestLockedClientProbeUsesBoundDaemonAndImmutableImage(t *testing.T) {
	record := filepath.Join(t.TempDir(), "calls")
	rt := serviceReviewRuntime(t, "")
	if err := os.WriteFile(rt.Name, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+fmt.Sprintf("%q", record)+"\necho native-canary\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	docker, err := runtime.BindDocker(t.Context(), rt, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	image := "sha256:" + strings.Repeat("a", 64)
	if out, err := lockedClientProbe(t.Context(), docker, image, t.TempDir(), "/tmp/h", "probe"); err != nil || string(out) != "native-canary\n" {
		t.Fatalf("parser canary = %q, %v", out, err)
	}
	before, err := os.ReadFile(record)
	if err != nil || !strings.Contains(string(before), "--network none") || !strings.Contains(string(before), image) {
		t.Fatalf("immutable offline probe args: %s, %v", before, err)
	}
	if _, err := lockedClientProbe(t.Context(), docker, "moving:latest", t.TempDir(), "/tmp/h", "probe"); err == nil {
		t.Fatal("mutable tag accepted")
	}
	t.Setenv("COOP_TEST_DAEMON", "replaced-daemon")
	if _, err := lockedClientProbe(t.Context(), docker, image, t.TempDir(), "/tmp/h", "probe"); err == nil {
		t.Fatal("probe crossed daemon replacement")
	}
	after, err := os.ReadFile(record)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("refused probe executed: %s, %v", after, err)
	}
	t.Setenv("COOP_TEST_DAEMON", "")
	if err := os.WriteFile(rt.Name, []byte("#!/bin/sh\necho native-canary\nexit 125\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := lockedClientProbe(t.Context(), docker, image, t.TempDir(), "/tmp/h", "probe"); err == nil {
		t.Fatal("runtime failure accepted because it printed a canary")
	}
}
