//go:build !cooplivetest

package cli

import (
	"os/exec"
	"testing"

	"github.com/AndrewDryga/coop/internal/liveprocess"
)

func TestDefaultBuildIgnoresLiveProcessEnvironment(t *testing.T) {
	t.Setenv(liveprocess.ControlFDEnv, "3")
	t.Setenv(liveprocess.ProcessDirEnv, "/untrusted/live-process-groups")
	t.Setenv(liveprocess.CleanupIDEnv, "untrusted-cleanup")
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := startACPProcess(cmd, "supervisor"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
