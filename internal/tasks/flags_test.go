package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/ui"
)

// Completing a task records which of its commits changed files that run on the host, the board
// marks the task until a human acknowledges it, and `coop tasks flags` lists and clears it. A
// task whose commits touch only ordinary code carries nothing.
func TestCompletionFlagsCommitsThatChangeWhatRunsOnTheHost(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	root := filepath.Join(repo, TasksRoot)
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	hooked := taskForLease(t, root, StateInProgress, "hooked")
	plain := taskForLease(t, root, StateInProgress, "plain")
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(".githooks/pre-commit", "#!/bin/sh\nexit 0\n")
	write("main.go", "package main\n")
	git("add", ".githooks/pre-commit", "main.go")
	git("commit", "-qm", "wire a hook\n\nCoop-Task: hooked")
	write("lib.go", "package main\n")
	git("add", "lib.go")
	git("commit", "-qm", "plain code\n\nCoop-Task: plain")

	for _, task := range []Item{hooked, plain} {
		if err := CompleteTrustedTask(root, task); err != nil {
			t.Fatalf("complete %s: %v", task.ID, err)
		}
	}
	flags, ok, err := ReadTaskFlags(filepath.Join(root, StateDone, "hooked"))
	if err != nil || !ok || len(flags.HostSurfaces) != 1 || flags.HostSurfaces[0].Path != ".githooks/pre-commit" ||
		!strings.Contains(flags.HostSurfaces[0].Reason, "git commit") || flags.Acknowledged {
		t.Fatalf("hooked task flags = %+v, ok=%v, err=%v; want the hook, unacknowledged", flags, ok, err)
	}
	if _, ok, err := ReadTaskFlags(filepath.Join(root, StateDone, "plain")); err != nil || ok {
		t.Fatalf("plain task carries flags: ok=%v err=%v", ok, err)
	}
	items, err := ReadTaskTree(root)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Item{}
	for _, item := range items {
		byID[item.ID] = item
	}
	if !byID["hooked"].HasFlags || byID["plain"].HasFlags {
		t.Fatalf("HasFlags: hooked=%v plain=%v", byID["hooked"].HasFlags, byID["plain"].HasFlags)
	}
	if markers := listMarkers(ui.For(os.Stdout), byID["hooked"]); !strings.Contains(markers, "changes what runs on your machine") {
		t.Fatalf("list markers for the flagged task = %q", markers)
	}
	if markers := listMarkers(ui.For(os.Stdout), byID["plain"]); strings.Contains(markers, "changes what runs") {
		t.Fatalf("list markers for the plain task = %q", markers)
	}

	captured, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = captured
	t.Cleanup(func() { os.Stdout = stdout })
	if code, err := tasksFolderFlags(root, nil); code != 0 || err != nil {
		t.Fatalf("tasks flags = (%d, %v)", code, err)
	}
	if code, err := tasksFolderFlags(root, []string{"hooked", "--ack"}); code != 0 || err != nil {
		t.Fatalf("tasks flags hooked --ack = (%d, %v)", code, err)
	}
	if code, err := tasksFolderFlags(root, []string{"plain"}); code != 0 || err != nil {
		t.Fatalf("tasks flags plain = (%d, %v)", code, err)
	}
	if code, err := tasksFolderFlags(root, []string{"nope"}); code != 1 || err == nil {
		t.Fatalf("tasks flags nope = (%d, %v); want a failure for an unknown task", code, err)
	}
	output, err := os.ReadFile(captured.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hooked  ⚠ changes what runs on your machine", ".githooks/pre-commit", "acknowledged — the flag is cleared", "plain carries no flags"} {
		if !strings.Contains(string(output), want) {
			t.Errorf("tasks flags output lacks %q:\n%s", want, output)
		}
	}
	flags, _, _ = ReadTaskFlags(filepath.Join(root, StateDone, "hooked"))
	if !flags.Acknowledged || flags.AcknowledgedBy == "" {
		t.Fatalf("flags after --ack = %+v", flags)
	}
	items, _ = ReadTaskTree(root)
	for _, item := range items {
		if item.ID == "hooked" && item.HasFlags {
			t.Fatal("acknowledged flags still mark the board")
		}
	}
}
