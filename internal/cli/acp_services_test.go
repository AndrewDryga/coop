package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
)

// An editor session's project services stay while any inner child it started still runs, warm or
// active, since that child may be using or starting them; a child of another session does not hold
// them, and once the session's own children are gone its project goes. The empty id names the
// development project, which a session never removes.
func TestACPServicesWaitForEveryChild(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	a := &app{cfg: &config.Config{}, rt: recordingRuntime(t, calls), rtSet: true}
	repo := t.TempDir()
	start := func() *exec.Cmd {
		child := exec.Command("sleep", "60")
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its own group, as spawnBox's children
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
		return child
	}
	warm, other := start(), start()
	a.acpChildren.add("session", warm.Process.Pid)
	a.acpChildren.add("other-session", other.Process.Pid)

	a.removeACPServices(repo, "session")
	a.removeACPServices(repo, "")
	if got := readFileString(calls); got != "" {
		t.Fatalf("services removed while a child of the session runs, or for the development project:\n%s", got)
	}
	_ = warm.Process.Kill()
	_ = warm.Wait()
	a.removeACPServices(repo, "session")
	if got := readFileString(calls); !strings.Contains(got, "label=com.docker.compose.project="+box.ComposeProjectFor(repo, "session")) {
		t.Fatalf("the session's project was not removed once its children were gone:\n%s", got)
	}
}

// spawnBox records every inner child it starts under its supervisor id, so the guard above sees a
// live child, and forgets it once the child has stopped.
func TestSpawnBoxRecordsItsChildForTheServicesGuard(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ctrl := acpctl.New(cfg, "codex", "", "", t.TempDir(), acpctl.Selection{}, nil, nil, acpHost())
	a := &app{cfg: cfg}
	child, err := a.spawnBox(context.Background(), shim, nil, "session", ctrl, agents.Target{Provider: "codex", Accounts: []string{"default"}}, "", true, io.Discard, forkspace.ExecutionRoleWarm)
	if err != nil {
		t.Fatal(err)
	}
	if !a.acpChildren.running("session") {
		child.Stop()
		t.Fatal("a running child of the session was not recorded")
	}
	child.Stop()
	if a.acpChildren.running("session") {
		t.Fatal("a stopped child still holds the session's services")
	}
}
