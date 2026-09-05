package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func contextTaskFixture(t *testing.T, first, second string) (string, *app) {
	t.Helper()
	repo := t.TempDir()
	writeUmbrellaProject(t, repo, "a", "b")
	ctxWrite(t, filepath.Join(repo, "AGENTS.md"), "canonical instructions\n")
	for i, id := range []string{first, second} {
		if id != "" {
			member := []string{"a", "b"}[i]
			ctxWrite(t, filepath.Join(repo, member, tasksRoot, stateTodo, id, "task.md"),
				"---\npaths: ["+member+"/file.go]\n---\n# Task\n")
		}
	}
	return repo, appForDerivedQueues(repo)
}

func contextTestGit(t *testing.T) string {
	t.Helper()
	bin, trace := t.TempDir(), filepath.Join(t.TempDir(), "git.trace")
	ctxWrite(t, trace, "")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COOP_CONTEXT_TEST_GIT_TRACE\"\nprintf ' M changed.go\\000'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONTEXT_TEST_GIT_TRACE", trace)
	t.Setenv("PATH", bin)
	return trace
}

func TestContextTaskQueueIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, id, want, problem string
		configured, flags                      []string
	}{
		{name: "duplicate exact", first: "shared", second: "shared", id: "shared", problem: "matches 2 tasks across the queues"},
		{name: "duplicate fragment", first: "one-shared", second: "two-shared", id: "shared", problem: "matches 2 tasks across the queues"},
		{name: "exact first", first: "shared", second: "two-shared", id: "shared", want: "a/file.go"},
		{name: "exact last", first: "one-shared", second: "shared", id: "shared", want: "b/file.go"},
		{name: "unique later", second: "two-shared", id: "shared", want: "b/file.go"},
		{name: "missing", first: "shared", second: "other", id: "absent", problem: "no task matching"},
		{name: "configured", first: "shared", second: "shared", id: "shared", configured: []string{"b/.agent/tasks"}, want: "b/file.go"},
		{name: "explicit spaced", first: "shared", second: "shared", id: "shared", configured: []string{"../outside"}, flags: []string{"--tasks", "b/.agent/tasks"}, want: "b/file.go"},
		{name: "explicit equals", first: "shared", second: "shared", id: "shared", flags: []string{"--tasks=a/.agent/tasks"}, want: "a/file.go"},
		{name: "repeated selectors", first: "shared", second: "other", id: "other", configured: []string{"../outside"}, flags: []string{"--tasks", "a/.agent/tasks", "--tasks=b/.agent/tasks"}, want: "b/file.go"},
		{name: "single queue missing id", first: "shared", id: "absent", flags: []string{"--tasks=a/.agent/tasks"}, problem: "no task matching"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, a := contextTaskFixture(t, tc.first, tc.second)
			a.cfg.TasksFiles = tc.configured
			trace := contextTestGit(t)
			args := append([]string{"--json", "--changed", "--task", tc.id}, tc.flags...)
			var code int
			var err error
			out := captureStdout(t, func() { code, err = a.cmdContext(args) })
			calls, readErr := os.ReadFile(trace)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.problem != "" {
				if code != 2 || err == nil || !strings.Contains(err.Error(), tc.problem) || out != "" || len(calls) != 0 {
					t.Fatalf("context=(%d, %v), output=%q, git=%q; want silent pre-Git refusal %q", code, err, out, calls, tc.problem)
				}
				_, pathErr := a.cmdTasks(append([]string{"path", tc.id}, tc.flags...))
				if pathErr == nil || pathErr.Error() != err.Error() {
					t.Fatalf("identity errors differ: context=%v, tasks path=%v", err, pathErr)
				}
				return
			}
			if code != 0 || err != nil || !strings.Contains(string(calls), "status --porcelain=v1") {
				t.Fatalf("context=(%d, %v), git=%q", code, err, calls)
			}
			var got ctxResult
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Scope, []string{"changed.go", tc.want}) {
				t.Fatalf("scope=%v, want changed.go then %s", got.Scope, tc.want)
			}
		})
	}
}

func TestContextTaskGrammarBeforeDiscovery(t *testing.T) {
	for _, args := range [][]string{
		{"--task"}, {"--task="}, {"--task", ""}, {"--task=--changed"},
		{"--task", "--changed"}, {"--task", "--tasks", "a/.agent/tasks", "shared"},
		{"--task", "--tasks=a/.agent/tasks", "shared"}, {"--tasks", "--task=shared", "file.go"},
		{"--task", "", "--task=shared"}, {"--task=", "--task=shared"},
		{"--task=shared", "--tasks"}, {"--task=shared", "--tasks="},
		{"--task=shared", "--tasks", ""}, {"--tasks=a/.agent/tasks", "file.go"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			repo, a := contextTaskFixture(t, "shared", "")
			// No repository or project work is needed to diagnose bad grammar.
			ctxWrite(t, filepath.Join(repo, ".agent", "project.yaml"), "subprojects: [\n")
			trace := contextTestGit(t)
			var code int
			var err error
			out := captureStdout(t, func() { code, err = a.cmdContext(append([]string{"--changed"}, args...)) })
			if code != 2 || err == nil || !strings.Contains(err.Error(), "--task") || out != "" {
				t.Fatalf("malformed context=(%d, %v), output=%q", code, err, out)
			}
			if calls, err := os.ReadFile(trace); err != nil || len(calls) != 0 {
				t.Fatalf("malformed syntax ran git: %q, %v", calls, err)
			}
		})
	}
}

func TestContextTaskMetadataRefusals(t *testing.T) {
	for _, kind := range []string{"duplicate state", "later symlink", "selected symlink"} {
		t.Run(kind, func(t *testing.T) {
			repo, a := contextTaskFixture(t, "shared", "")
			switch kind {
			case "duplicate state":
				ctxWrite(t, filepath.Join(repo, "a", tasksRoot, stateInProgress, "shared", "task.md"), "# Duplicate\n")
			default:
				member, id := "b", "later"
				if kind == "selected symlink" {
					member, id = "a", "shared"
				}
				path := filepath.Join(repo, member, tasksRoot, stateTodo, id, "task.md")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if kind == "selected symlink" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(filepath.Join(repo, "AGENTS.md"), path); err != nil {
					t.Fatal(err)
				}
			}
			trace := contextTestGit(t)
			var code int
			var err error
			out := captureStdout(t, func() { code, err = a.cmdContext([]string{"--json", "--changed", "--task=shared"}) })
			if code != 2 || err == nil || out != "" {
				t.Fatalf("unsafe metadata context=(%d, %v), output=%q", code, err, out)
			}
			if calls, err := os.ReadFile(trace); err != nil || len(calls) != 0 {
				t.Fatalf("unsafe metadata ran git: %q, %v", calls, err)
			}
		})
	}
}

func TestContextTaskSelectionPreservesScopeOrder(t *testing.T) {
	repo, a := contextTaskFixture(t, "shared", "shared")
	ctxWrite(t, filepath.Join(repo, ".agent", "project.yaml"),
		"subprojects: [a, b]\ncontext:\n  routes:\n    - paths: ['**/*.go']\n      include: [.agent/kb/code.md]\n")
	ctxWrite(t, filepath.Join(repo, ".agent", "kb", "code.md"), "code guidance\n")
	ctxWrite(t, filepath.Join(repo, "b", tasksRoot, stateTodo, "shared", "task.md"),
		"---\npaths: [b/file.go, changed.go, explicit.go, a]\n---\n# Task\n")
	t.Chdir(filepath.Join(repo, "a"))
	contextTestGit(t)
	var code int
	var err error
	out := captureStdout(t, func() {
		code, err = a.cmdContext([]string{"--json", "explicit.go", "--changed", "--task=absent", "--task", "shared", "--tasks=b/.agent/tasks"})
	})
	if code != 0 || err != nil {
		t.Fatalf("context=(%d, %v)", code, err)
	}
	var got ctxResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Scope, []string{"a", "explicit.go", "changed.go", "b/file.go"}) {
		t.Fatalf("scope order=%v", got.Scope)
	}
	if len(got.Files) != 2 || got.Files[1].Reason != "route **/*.go → explicit.go" {
		t.Fatalf("route reason/order changed: %+v", got.Files)
	}
}

func TestContextWithoutTaskIgnoresQueues(t *testing.T) {
	repo, a := contextTaskFixture(t, "shared", "shared")
	a.cfg.TasksFiles = []string{"../outside"}
	var code int
	var err error
	captureStdout(t, func() { code, err = a.cmdContext([]string{"--json", "file.go"}) })
	if code != 0 || err != nil {
		t.Fatalf("path-only context inspected task queues: (%d, %v)", code, err)
	}
	ctxWrite(t, filepath.Join(repo, ".agent", "project.yaml"), "context: [\n")
	if code, err := a.cmdContext([]string{"--task=shared", "--tasks=a/.agent/tasks"}); code != 2 || err == nil {
		t.Fatalf("explicit queue bypassed project config: (%d, %v)", code, err)
	}
}
