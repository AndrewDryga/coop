package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// Answering no to a coop confirmation exits 1 and says what was kept: nothing changed, and nothing
// was wrong with the command, which exit 2, a usage error, would say. Each command's own question
// is answered by a stand-in terminal, and what it asked to remove is still there afterwards.
func TestEveryDeclinedConfirmationExitsOne(t *testing.T) {
	task := func(t *testing.T, state, id string) (*app, string) {
		t.Helper()
		repo := t.TempDir()
		dir := filepath.Join(repo, ".agent", "tasks", state, id)
		ctxWrite(t, filepath.Join(dir, "task.md"), "# "+id+"\n")
		return &app{cfg: &config.Config{RepoOverride: repo}}, dir
	}
	fork := func(t *testing.T) (*app, string) {
		t.Helper()
		repo := initRepo(t)
		ws, err := forkspace.Setup(repo, "a")
		if err != nil {
			t.Fatal(err)
		}
		// A runtime that does nothing, so a question is all a command gets to.
		runtimeCLI := filepath.Join(t.TempDir(), "runtime")
		if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}, rt: runtime.Runtime{Name: runtimeCLI}, rtSet: true}, ws
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (run func() (int, error), kept func() bool)
		said  string
	}{
		{"coop tasks rm", func(t *testing.T) (func() (int, error), func() bool) {
			a, dir := task(t, "00_todo", "2026-11-06-login")
			return func() (int, error) { return a.cmdTasks([]string{"rm", "2026-11-06-login"}) }, func() bool { return pathExists(dir) }
		}, "Cancelled. No tasks deleted."},
		{"coop tasks rm --all-done", func(t *testing.T) (func() (int, error), func() bool) {
			a, dir := task(t, "99_done", "2026-11-06-shipped")
			return func() (int, error) { return a.cmdTasks([]string{"rm", "--all-done"}) }, func() bool { return pathExists(dir) }
		}, "Cancelled. No tasks deleted."},
		{"coop backlog rm", func(t *testing.T) (func() (int, error), func() bool) {
			a, dir := task(t, "xx_backlog", "2026-11-06-idea")
			return func() (int, error) { return a.cmdBacklog([]string{"rm", "2026-11-06-idea"}) }, func() bool { return pathExists(dir) }
		}, "Cancelled. The idea was kept."},
		{"coop credentials rm", func(t *testing.T) (func() (int, error), func() bool) {
			cfg := freshConfig(t)
			writeAccount(t, cfg, "codex", "personal", codexReadyCredential)
			writeAccount(t, cfg, "codex", "old-work", codexReadyCredential)
			setDefault(t, cfg, "codex", "personal")
			a := &app{cfg: cfg}
			return func() (int, error) { return a.cmdCredentials([]string{"codex", "old-work", "rm"}) },
				func() bool { return !box.NativeAccountRemoved(cfg, "codex", "old-work") }
		}, "Cancelled. Nothing was removed."},
		{"coop fork rm", func(t *testing.T) (func() (int, error), func() bool) {
			a, ws := fork(t)
			return func() (int, error) { return a.cmdFork([]string{"rm", "a"}) }, func() bool { return pathExists(ws) }
		}, "Cancelled. The fork was kept."},
		{"coop fork merge --all", func(t *testing.T) (func() (int, error), func() bool) {
			a, ws := fork(t)
			return func() (int, error) { return a.cmdFork([]string{"merge", "--all"}) }, func() bool { return pathExists(ws) }
		}, "Cancelled. No fork was merged or deleted."},
		{"coop fork --fresh", func(t *testing.T) (func() (int, error), func() bool) {
			a, ws := fork(t)
			return func() (int, error) { return a.cmdFork([]string{"a", "--fresh"}) }, func() bool { return pathExists(ws) }
		}, "Cancelled. The fork was kept."},
		{"coop down --delete-volumes", func(t *testing.T) (func() (int, error), func() bool) {
			repo := serviceProject(t)
			shim := composeShim{services: []string{"db", "redis"}, volumes: projectVolumes(t, repo)}
			a := &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
			return func() (int, error) { return a.cmdDown([]string{"--delete-volumes"}) }, func() bool { return true }
		}, "Cancelled. Nothing was stopped or deleted."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, kept := tc.setup(t)
			// Two seams stand in for the one terminal: the gate's own, and the reader coop down and
			// coop fork --fresh ask through.
			t.Cleanup(ui.AnswerConfirmationsFrom(strings.NewReader("n\n")))
			typedAnswers(t, "n")
			var code int
			var err error
			out := captureTerminal(t, func() { code, err = run() })
			if code != 1 || !errors.Is(err, ui.ErrReported) || !strings.Contains(out, tc.said) {
				t.Errorf("declined %s = (%d, %v), want exit 1 and %q:\n%s", tc.name, code, err, tc.said, out)
			}
			if !kept() {
				t.Errorf("declined %s still removed what it asked about", tc.name)
			}
		})
	}
}
