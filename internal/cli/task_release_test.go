package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

func TestIntegrationUmbrellaRelease(t *testing.T) {
	for _, selection := range []string{"derived", "configured", "explicit", "explicit-multiple", "exact-over-fragment"} {
		t.Run(selection, func(t *testing.T) {
			repo := t.TempDir()
			t.Setenv(tasks.TestLeaseAuthorityRootEnv, t.TempDir())
			writeUmbrellaProject(t, repo, "a", "b")
			rels := []string{"a/" + tasksRoot, "b/" + tasksRoot}
			id := "2026-09-05-release-me"
			if selection == "exact-over-fragment" {
				id = "release-me"
			}
			root := filepath.Join(repo, rels[1])
			writeTaskFile(t, filepath.Join(root, stateTodo, id, "task.md"), "# Keep this task\n")
			a := appForDerivedQueues(repo)
			var flags []string
			switch selection {
			case "configured":
				a.cfg.TasksFiles = rels
			case "explicit":
				// Explicit selection wins over even a conflicting configured queue.
				a.cfg.TasksFiles = []string{rels[0]}
				flags = []string{"--tasks", rels[1]}
			case "explicit-multiple":
				a.cfg.TasksFiles = []string{"unselected/.agent/tasks"}
				flags = []string{"--tasks", rels[0], "--tasks=" + rels[1]}
			case "exact-over-fragment":
				writeTaskFile(t, filepath.Join(repo, rels[0], stateTodo, "2026-09-05-release-me", "task.md"), "# Other task\n")
			}
			run := func(verb string) {
				t.Helper()
				code, err := a.cmdTasks(append(append([]string{}, flags...), verb, "release-me"))
				if code != 0 || err != nil {
					t.Fatalf("%s: code=%d err=%v", verb, code, err)
				}
			}
			run("claim")
			if owner, ok, err := tasks.ReadTaskOwnerRecord(root, id); err != nil || !ok || owner.Kind != tasks.TaskOwnerHuman {
				t.Fatalf("claim owner=%+v exists=%v err=%v", owner, ok, err)
			}
			run("release")
			if _, ok, err := tasks.ReadTaskOwnerRecord(root, id); err != nil || ok {
				t.Fatalf("release retained owner: exists=%v err=%v", ok, err)
			}
			// Release returns the folder to todo, instructions and handoff intact, in the queue
			// that held it — never in the other member's.
			if data, err := os.ReadFile(filepath.Join(root, stateTodo, id, "task.md")); err != nil || !strings.Contains(string(data), "Keep this task") {
				t.Fatalf("released task in todo = %q, %v", data, err)
			}
			returned := snapshotInitTree(t, repo)
			run("release") // Already in todo and unclaimed is a successful no-op.
			assertTaskTreeUnchanged(t, repo, returned)
		})
	}
}

func assertTaskTreeUnchanged(t *testing.T, root string, before map[string]initTreeEntry) {
	t.Helper()
	after := snapshotInitTree(t, root)
	if len(after) != len(before) {
		t.Errorf("task operation changed inventory: before=%d after=%d", len(before), len(after))
	}
	for path, old := range before {
		current, ok := after[path]
		if !ok || old.data != current.data || old.link != current.link || old.info.Mode() != current.info.Mode() || !os.SameFile(old.info, current.info) {
			t.Errorf("task operation changed retained path %s", filepath.Join(root, path))
		}
	}
}

func TestIntegrationUmbrellaReleaseRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, secondID, state, want string
		args                        []string
		code                        int
	}{
		{name: "duplicate exact", secondID: "2026-09-05-shared", args: []string{"2026-09-05-shared"}, code: 1, want: "matches 2 tasks across the queues"},
		{name: "duplicate fragment", secondID: "2026-09-06-shared", args: []string{"shared"}, code: 1, want: "matches 2 tasks across the queues"},
		{name: "missing identity", args: []string{"absent"}, code: 1, want: "no task matching"},
		{name: "missing argument", code: 2, want: `Missing task ID for "coop tasks release"`},
		{name: "extra argument", args: []string{"shared", "extra"}, code: 2, want: `Unexpected argument "extra" for "coop tasks release"`},
		{name: "unknown flag", args: []string{"shared", "--force"}, code: 2, want: `Unknown option "--force" for "coop tasks release"`},
		{name: "wrong state", state: stateBlocked, args: []string{"shared"}, code: 1, want: "Resolve its decision before returning it to todo."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, authority := t.TempDir(), t.TempDir()
			t.Setenv(tasks.TestLeaseAuthorityRootEnv, authority)
			writeUmbrellaProject(t, repo, "a", "b")
			a := appForDerivedQueues(repo)
			// tc.state is the state the fixture parks the task in; the default is a claimed,
			// in-progress task — the one release actually acts on.
			for i, id := range []string{"2026-09-05-shared", tc.secondID} {
				if id == "" {
					continue
				}
				rel := []string{"a/" + tasksRoot, "b/" + tasksRoot}[i]
				state := tc.state
				if state == "" {
					state = stateTodo
				}
				writeTaskFile(t, filepath.Join(repo, rel, state, id, "task.md"), "# Preserve me\n")
				if tc.state == stateBlocked {
					writeTaskFile(t, filepath.Join(repo, rel, state, id, "decision.md"), "# Decision: pick one?\n")
				}
				if tc.state == "" {
					if code, err := a.cmdTasks([]string{"claim", id, "--tasks", rel}); code != 0 || err != nil {
						t.Fatalf("claim fixture: %d, %v", code, err)
					}
				}
			}
			before, authorityBefore := snapshotInitTree(t, repo), snapshotInitTree(t, authority)
			code, err := a.cmdTasks(append([]string{"release"}, tc.args...))
			if code != tc.code || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("release=%d, %v; want %d containing %q", code, err, tc.code, tc.want)
			}
			assertTaskTreeUnchanged(t, repo, before)
			assertTaskTreeUnchanged(t, authority, authorityBefore)
			if tc.secondID != "" {
				// An explicit selector resolves the ambiguity without
				// releasing the other queue's human claim.
				code, err := a.cmdTasks([]string{"release", "2026-09-05-shared", "--tasks", "a/" + tasksRoot})
				if code != 0 || err != nil {
					t.Fatalf("selected release: %d, %v", code, err)
				}
				if _, ok, err := tasks.ReadTaskOwnerRecord(filepath.Join(repo, "b", tasksRoot), tc.secondID); err != nil || !ok {
					t.Fatalf("other claim lost: %v, %v", ok, err)
				}
			}
		})
	}
}
