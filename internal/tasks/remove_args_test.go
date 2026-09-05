package tasks

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

type removalSnapshotEntry struct {
	info os.FileInfo
	body string
}

func removalSnapshot(t *testing.T, root string) map[string]removalSnapshotEntry {
	t.Helper()
	entries := map[string]removalSnapshotEntry{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var body []byte
		if !entry.IsDir() {
			body, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		entries[path] = removalSnapshotEntry{info, string(body)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func assertRemovalSnapshot(t *testing.T, root string, before map[string]removalSnapshotEntry) {
	t.Helper()
	after := removalSnapshot(t, root)
	if len(before) != len(after) {
		t.Errorf("removal changed directory inventory: before=%d after=%d", len(before), len(after))
	}
	for path, old := range before {
		current, ok := after[path]
		if !ok || old.body != current.body || old.info.Mode() != current.info.Mode() || !os.SameFile(old.info, current.info) {
			t.Errorf("removal changed retained path %s", path)
		}
	}
}

func TestTaskRemovalRejectsMalformedArgsBeforeTouchingState(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"mixed ID and archive", []string{"keep", "--all-done"}},
		{"multiple IDs", []string{"keep", "done"}},
		{"unknown before archive", []string{"--dry-run", "--all-done"}},
		{"unknown after archive", []string{"--all-done", "--force"}},
		{"unknown after ID", []string{"keep", "--bogus"}},
		{"empty ID", []string{""}},
		{"empty ID and archive", []string{"", "--all-done"}},
		{"lone dash", []string{"-"}},
		{"dash and archive", []string{"-", "--all-done"}},
		{"unsupported separator", []string{"--", "--all-done"}},
	}
	for _, tc := range cases {
		for _, aggregate := range []bool{false, true} {
			for _, populated := range []bool{false, true} {
				for _, yes := range []bool{false, true} {
					name := tc.name + map[bool]string{false: "/single", true: "/aggregate"}[aggregate] + map[bool]string{false: "/empty", true: "/populated"}[populated] + map[bool]string{false: "/prompt", true: "/yes"}[yes]
					t.Run(name, func(t *testing.T) {
						repo, authority := t.TempDir(), t.TempDir()
						t.Setenv(TestLeaseAuthorityRootEnv, authority)
						rels := []string{"a/.agent/tasks", "b/.agent/tasks"}
						for _, rel := range rels {
							root := filepath.Join(repo, rel)
							writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-02-keep", "task.md"), "# retain this task\n")
							if populated {
								writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-01-done", "task.md"), "# retain this archive\n")
							}
						}
						windows, err := BeginCompletionWindows([]string{filepath.Join(repo, rels[0]), filepath.Join(repo, rels[1])})
						if err != nil {
							t.Fatal(err)
						}
						if err := windows.Abandon(); err != nil {
							t.Fatal(err)
						}
						before, authorityBefore := removalSnapshot(t, repo), removalSnapshot(t, authority)
						args := append([]string{}, tc.args...)
						if yes {
							args = append(args, "--yes")
						}
						var code int
						if aggregate {
							code, err = tasksAcrossQueues(repo, rels, "rm", append([]string{"rm"}, args...))
						} else {
							code, err = tasksFolderRemove(filepath.Join(repo, rels[0]), args)
						}
						if code != 2 || err == nil || (!strings.Contains(err.Error(), "usage:") && !strings.Contains(err.Error(), "unknown flag") && !strings.Contains(err.Error(), "too many arguments")) {
							t.Errorf("malformed removal = %d, %v; want syntax refusal before discovery/confirmation", code, err)
						}
						assertRemovalSnapshot(t, repo, before)
						assertRemovalSnapshot(t, authority, authorityBefore)
					})
				}
			}
		}
	}
}

func TestTaskRemovalPreservesValidGrammar(t *testing.T) {
	for _, aggregate := range []bool{false, true} {
		for _, mode := range []string{"exact", "substring", "archive", "empty archive", "ambiguous ID", "no implicit removal"} {
			t.Run(map[bool]string{false: "single/", true: "aggregate/"}[aggregate]+mode, func(t *testing.T) {
				repo := t.TempDir()
				t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
				rels := []string{"a/.agent/tasks", "b/.agent/tasks"}
				ids := []string{"2026-01-01-first", "2026-01-02-second"}
				for i, rel := range rels {
					root := filepath.Join(repo, rel)
					writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-03-keep", "task.md"), "# keep\n")
					if mode != "empty archive" {
						writeTaskFile(t, filepath.Join(root, StateDone, ids[i], "task.md"), "# done\n")
					}
					if mode == "ambiguous ID" {
						writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-04-shared", "task.md"), "# shared\n")
					}
				}
				cfg := &config.Config{RepoOverride: repo, TasksFiles: rels[:1]}
				if aggregate {
					cfg.TasksFiles = rels
				}
				args := []string{"rm", ids[0], "--yes"}
				switch mode {
				case "substring":
					args = []string{"rm", "-y", "first", "--yes", "-y"}
				case "archive":
					args = []string{"rm", "--yes", "--all-done", "-y", "--all-done"}
				case "empty archive":
					args = []string{"rm", "--all-done"} // still a no-op without confirmation
				case "ambiguous ID":
					args = []string{"rm", "shared", "--yes"}
				case "no implicit removal":
					args = []string{"--all-done", "--yes"}
				}
				before := removalSnapshot(t, repo)
				code, err := CmdTasks(Host{}, cfg, args)
				if mode == "no implicit removal" || mode == "ambiguous ID" && aggregate {
					if code == 0 || err == nil {
						t.Fatalf("ambiguous/implicit removal = %d, %v", code, err)
					}
					assertRemovalSnapshot(t, repo, before)
					return
				}
				if code != 0 || err != nil {
					t.Fatalf("valid removal = %d, %v", code, err)
				}
				for i, rel := range rels {
					root := filepath.Join(repo, rel)
					if data, err := os.ReadFile(filepath.Join(root, StateTodo, "2026-01-03-keep", "task.md")); err != nil || string(data) != "# keep\n" {
						t.Fatalf("non-target task lost: %q, %v", data, err)
					}
					wantDone := mode != "empty archive" && (mode == "ambiguous ID" || i == 1 && (mode != "archive" || !aggregate))
					_, statErr := os.Stat(filepath.Join(root, StateDone, ids[i]))
					if (statErr == nil) != wantDone {
						t.Errorf("retained archive %s: exists=%v want=%v", ids[i], statErr == nil, wantDone)
					}
				}
			})
		}
	}
}

func TestTaskRemovalSyntaxPrecedesProjectAndQueueDiscovery(t *testing.T) {
	for _, broken := range []string{"project", "queue"} {
		t.Run(broken, func(t *testing.T) {
			repo := t.TempDir()
			cfg := &config.Config{RepoOverride: repo}
			if broken == "project" {
				writeTaskFile(t, filepath.Join(repo, ".agent", "project.yaml"), "invalid: [\n")
			} else {
				writeTaskFile(t, filepath.Join(repo, ".agent", "tasks"), "not a directory\n")
				cfg.TasksFiles = []string{".agent/tasks"}
			}
			before := removalSnapshot(t, repo)
			code, err := CmdTasks(Host{}, cfg, []string{"rm", "--all-done", "--dry-run", "--yes"})
			if code != 2 || err == nil || !strings.Contains(err.Error(), "unknown flag") {
				t.Fatalf("invalid args reached broken %s: %d, %v", broken, code, err)
			}
			assertRemovalSnapshot(t, repo, before)
		})
	}
}
