package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

// Fatal must run in an isolated driving test, not a failing subtest of the keeper.
func TestTaskHelperAbortProbe(t *testing.T) {
	mode := os.Getenv("COOP_TEST_HELPER_ABORT")
	if mode == "" {
		return
	}
	root := os.Getenv("COOP_TEST_HELPER_ABORT_ROOT")
	holder := startRefAuthorityProcess(t, filepath.Join(root, "config"), filepath.Join(root, "repo"))
	if err := os.WriteFile(filepath.Join(root, "child.pid"), []byte(fmt.Sprint(holder.cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if mode == "ready" && holder.readLine(t) != "LOCKED" {
		t.Fatal("helper did not acquire its lock")
	}
	t.Fatal("deliberate abort after helper ownership")
}

func TestTaskHelperCleanupSurvivesAnAbortedDrivingTest(t *testing.T) {
	for _, mode := range []string{"started", "ready"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			result := procharness.Run(t.Context(), procharness.Command{
				Path: os.Args[0], Args: []string{"-test.run=^TestTaskHelperAbortProbe$"},
				Env: append(os.Environ(), "COOP_TEST_HELPER_ABORT="+mode, "COOP_TEST_HELPER_ABORT_ROOT="+root),
			})
			if result.Err != nil || result.ExitCode != 1 || !strings.Contains(result.Stdout, "deliberate abort after helper ownership") {
				t.Fatalf("abort probe = %+v", result)
			}
			body, err := os.ReadFile(filepath.Join(root, "child.pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(body))
			if err != nil || pid <= 0 {
				t.Fatalf("child pid %q: %v", body, err)
			}
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatalf("aborted driver's child %d survived: %v", pid, err)
			}
			release, err := LockRefAuthority(&config.Config{ConfigDir: filepath.Join(root, "config")}, filepath.Join(root, "repo"))
			if err != nil {
				t.Fatalf("aborted helper stranded ref authority: %v", err)
			}
			release()
		})
	}
}
