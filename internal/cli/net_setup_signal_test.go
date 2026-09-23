package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestNetSetupInterruptCancelsWork(t *testing.T) {
	if os.Getenv("COOP_TEST_NET_SETUP_INTERRUPT") == "child" {
		ready := os.Getenv("COOP_TEST_NET_SETUP_READY")
		err := withNetSetupSignals(func(ctx context.Context) error {
			if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		})
		fmt.Printf("setup returned: %v\n", err)
		return
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNetSetupInterruptCancelsWork$")
			cmd.Env = append(os.Environ(), "COOP_TEST_NET_SETUP_INTERRUPT=child", "COOP_TEST_NET_SETUP_READY="+ready)
			output := new(strings.Builder)
			cmd.Stdout, cmd.Stderr = output, output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			wait.For(t, "network setup readiness", func() bool {
				_, err := os.Stat(ready)
				return err == nil
			})
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil || !strings.Contains(output.String(), "setup returned: context canceled") {
				t.Fatalf("setup did not cleanly cancel on %s: %v\n%s", sig, err, output)
			}
		})
	}
}
