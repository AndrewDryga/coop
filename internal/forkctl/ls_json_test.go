package forkctl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// coop fork ls --json reports the root workspace (even with no forks) and its per-port serve URLs,
// each derived from project.HostPort of the WORKSPACE path — so host tooling discovers URLs without
// reproducing the port hash.
func TestForkLsJSON(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("serve:\n  ports: [4000]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	out := captureStdout(t, func() {
		if code, err := c.ForkLs([]string{"--json"}); code != 0 || err != nil {
			t.Fatalf("fork ls --json: (%d, %v)", code, err)
		}
	})
	var got struct {
		Workspaces []struct {
			Name  string            `json:"name"`
			Path  string            `json:"path"`
			Serve map[string]string `json:"serve"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatal(err)
	}
	if _, present := envelope["problems"]; present {
		t.Fatalf("healthy fork list emitted a null/empty problems field: %s", out)
	}
	if len(got.Workspaces) != 1 || got.Workspaces[0].Name != "root" {
		t.Fatalf("want a single root workspace, got %+v", got.Workspaces)
	}
	want := fmt.Sprintf("http://localhost:%d", project.HostPort(repo, 4000))
	if got.Workspaces[0].Serve["4000"] != want {
		t.Errorf("serve URL for 4000 = %q, want %q", got.Workspaces[0].Serve["4000"], want)
	}
}

func TestForkLsJSONRejectsInvalidProjectWithoutOutputOrRuntime(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("serve:\n  portz: [4000]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtimeCalls := 0
	c := &Control{
		cfg: &config.Config{RepoOverride: repo},
		host: Host{EnsureRuntime: func() (runtime.Runtime, error) {
			runtimeCalls++
			return runtime.Runtime{}, nil
		}},
	}
	var code int
	var runErr error
	out := captureStdout(t, func() { code, runErr = c.ForkLs([]string{"--json"}) })
	if code != -1 || runErr == nil || !strings.Contains(runErr.Error(), project.File) {
		t.Fatalf("fork ls --json = (%d, %v), want policy error", code, runErr)
	}
	if out != "" {
		t.Fatalf("invalid project emitted partial JSON: %q", out)
	}
	if runtimeCalls != 0 {
		t.Fatalf("invalid project resolved runtime %d time(s)", runtimeCalls)
	}
}

func TestForkLsJSONRejectsBrokenForkRootWithoutOutput(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(forkspace.Home(repo), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	var code int
	var runErr error
	out := captureStdout(t, func() { code, runErr = c.ForkLs([]string{"--json"}) })
	if code != -1 || runErr == nil || !strings.Contains(runErr.Error(), forkspace.Home(repo)) {
		t.Fatalf("fork ls --json = (%d, %v), want fork discovery error", code, runErr)
	}
	if out != "" {
		t.Fatalf("broken fork discovery emitted partial JSON: %q", out)
	}
}

func TestForkLsJSONIncludesLiveSandboxCounts(t *testing.T) {
	repo := t.TempDir()
	workspace := forkspace.Workspace(repo, "active")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "active")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "active")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	execution, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{
		Kind: forkspace.ExecutionForkACP, Role: forkspace.ExecutionRoleActive,
		Workspace: workspace, Fork: &identity, SourceID: "live-sandbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, execution) })
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	out := captureStdout(t, func() {
		if code, err := c.ForkLs([]string{"--json"}); code != 0 || err != nil {
			t.Fatalf("fork ls --json: (%d, %v)", code, err)
		}
	})
	var got struct {
		Workspaces []struct {
			Name   string `json:"name"`
			Status *struct {
				State           string `json:"state"`
				ActiveSandboxes int    `json:"active_sandboxes"`
			} `json:"status"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Workspaces) != 2 || got.Workspaces[1].Name != "active" || got.Workspaces[1].Status == nil ||
		got.Workspaces[1].Status.State != "active" || got.Workspaces[1].Status.ActiveSandboxes != 1 {
		t.Fatalf("active fork JSON status = %+v", got.Workspaces)
	}
}

func TestForkLsJSONIncludesForkStatus(t *testing.T) {
	repo := t.TempDir()
	workspace := forkspace.Workspace(repo, "legacy")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	out := captureStdout(t, func() {
		if code, err := c.ForkLs([]string{"--json"}); code != 0 || err != nil {
			t.Fatalf("fork ls --json: (%d, %v)", code, err)
		}
	})
	var got struct {
		Workspaces []struct {
			Name   string `json:"name"`
			Status *struct {
				State  string `json:"state"`
				Legacy bool   `json:"legacy_generation"`
			} `json:"status"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Workspaces) != 2 || got.Workspaces[1].Name != "legacy" || got.Workspaces[1].Status == nil ||
		got.Workspaces[1].Status.State != "legacy" || !got.Workspaces[1].Status.Legacy {
		t.Fatalf("legacy fork JSON status = %+v", got.Workspaces)
	}
}
