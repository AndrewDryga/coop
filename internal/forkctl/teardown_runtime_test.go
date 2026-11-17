package forkctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// forkWithServices makes a project whose committed compose file every fork inherits, plus a fork
// of it, and puts a recording "stubruntime" on PATH. It returns the repo and the record path.
func forkWithServices(t *testing.T, name string) (string, string) {
	t.Helper()
	stubDir := t.TempDir()
	record := filepath.Join(stubDir, "invocations")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>" + record + "\n"
	if err := os.WriteFile(filepath.Join(stubDir, "stubruntime"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := initRepo(t)
	compose := filepath.Join(repo, filepath.FromSlash(project.DefaultCompose))
	if err := os.MkdirAll(filepath.Dir(compose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compose, []byte("services:\n  db:\n    image: postgres:18.4-alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", project.DefaultCompose)
	git(t, repo, "commit", "-qm", "services")
	if _, err := forkspace.Setup(repo, name); err != nil {
		t.Fatal(err)
	}
	return repo, record
}

func stubRuntimeHost() Host {
	return Host{EnsureRuntime: func() (runtime.Runtime, error) { return runtime.Runtime{Name: "stubruntime"}, nil }}
}

func requireServicesDown(t *testing.T, record string) {
	t.Helper()
	got, err := os.ReadFile(record)
	if err != nil || !strings.Contains(string(got), "down") {
		t.Fatalf("the fork's services were never stopped (record %q, %v)", got, err)
	}
}

// Fork commands start with no runtime, so a removal that never detected one read the zero runtime
// as "nothing to stop", deleted the compose file, and left the fork's services running.
func TestForkRmDetectsRuntimeAndStopsServices(t *testing.T) {
	repo, record := forkWithServices(t, "svc")
	c := New(&config.Config{RepoOverride: repo}, runtime.Runtime{}, stubRuntimeHost())
	if code, err := c.ForkRm([]string{"svc", "--yes"}); code != 0 || err != nil {
		t.Fatalf("fork rm = (%d, %v)", code, err)
	}
	requireServicesDown(t, record)
	if pathExists(forkspace.Workspace(repo, "svc")) {
		t.Fatal("workspace was not removed")
	}
}

func TestForkMergeWithoutGateStopsServicesOfDeletedFork(t *testing.T) {
	repo, record := forkWithServices(t, "svc")
	c := New(&config.Config{RepoOverride: repo}, runtime.Runtime{}, stubRuntimeHost())
	if code, err := c.ForkMerge([]string{"svc", "--yes"}); code != 0 || err != nil {
		t.Fatalf("fork merge = (%d, %v)", code, err)
	}
	requireServicesDown(t, record)
	if pathExists(forkspace.Workspace(repo, "svc")) {
		t.Fatal("workspace was not removed")
	}
}

// A runtime that is configured but does not answer may still be running the fork's services, so
// the fork stays, as after any failed teardown, unless the person forces it.
func TestForkRmKeepsForkWhenRuntimeIsUnreachable(t *testing.T) {
	repo, _ := forkWithServices(t, "svc")
	host := Host{EnsureRuntime: func() (runtime.Runtime, error) { return runtime.Runtime{}, errors.New("daemon down") }}
	c := New(&config.Config{RepoOverride: repo, RuntimeName: "stubruntime"}, runtime.Runtime{}, host)
	code, err := c.ForkRm([]string{"svc", "--yes"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "daemon down") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("fork rm = (%d, %v), want a kept fork naming the runtime error and --force", code, err)
	}
	if !pathExists(forkspace.Workspace(repo, "svc")) {
		t.Fatal("fork was deleted although its services may still run")
	}
	if code, err := c.ForkRm([]string{"svc", "--yes", "--force"}); code != 0 || err != nil {
		t.Fatalf("fork rm --force = (%d, %v)", code, err)
	}
	if pathExists(forkspace.Workspace(repo, "svc")) {
		t.Fatal("forced removal kept the workspace")
	}
}
