package taskmcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

func TestCompletionEvidenceHandoff(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	for _, assigned := range []string{"t1", ""} {
		t.Run("assigned="+assigned, func(t *testing.T) {
			for _, retained := range []bool{false, true} {
				root := queue(t, map[string]string{"t1": tasks.StateInProgress})
				dir := filepath.Join(root, tasks.StateInProgress, "t1")
				for _, name := range []string{"tmp", "artifacts"} {
					if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				const log = "check returned 0; retained output is not attestation\n"
				if err := os.WriteFile(filepath.Join(dir, "tmp", "gate.log"), []byte(log), 0o600); err != nil {
					t.Fatal(err)
				}
				if retained {
					if err := os.WriteFile(filepath.Join(dir, "artifacts", "gate.log"), []byte(log), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink("../tmp/gate.log", filepath.Join(dir, "artifacts", "dangling.log")); err != nil {
					t.Fatal(err)
				}
				sess := newSession(t, newServer(t, root, assigned))
				// Refused completion does not consume evidence or issue a success receipt.
				refused := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1"})
				if strings.Contains(refused, completionEvidenceHandoff) {
					t.Fatal("refusal emitted successful handoff")
				}
				if _, err := os.Stat(filepath.Join(dir, "tmp", "gate.log")); err != nil {
					t.Fatalf("refusal lost scratch: %v", err)
				}
				finishChecklist(t, root, "t1")
				for call := 0; call < 2; call++ {
					text := sess.mustCall("tasks_complete", map[string]any{"id": "t1"})
					for _, want := range []string{"Host finalization removes tmp/", "never cite tmp/ as retained evidence", "or say the full log was not retained", "not independent proof", "write nothing more"} {
						if !strings.Contains(text, want) {
							t.Fatalf("handoff missing %q: %s", want, text)
						}
					}
					if strings.Contains(text, "gate.log") || strings.Contains(text, "dangling.log") {
						t.Fatal("receipt invented an artifact retention claim")
					}
				}
				item, ok, err := tasks.CurrentTask(root, "t1")
				if err != nil || !ok {
					t.Fatalf("done task missing: %v", err)
				}
				if assigned != "" {
					if _, err := os.Stat(filepath.Join(item.Dir, "tmp", "gate.log")); err != nil {
						t.Fatalf("assigned move removed scratch before host finalization: %v", err)
					}
				}
				if err := tasks.CompleteTrustedTask("", root, item); err != nil {
					t.Fatalf("host finalization: %v", err)
				}
				done := filepath.Join(root, tasks.StateDone, "t1")
				if _, err := os.Stat(filepath.Join(done, "tmp")); !os.IsNotExist(err) {
					t.Fatalf("scratch survived finalization: %v", err)
				}
				body, err := os.ReadFile(filepath.Join(done, "artifacts", "gate.log"))
				if retained && (err != nil || string(body) != log) || !retained && !os.IsNotExist(err) {
					t.Fatalf("wrong retained-artifact result: %q, %v", body, err)
				}
				if !retained {
					if _, err := os.Stat(filepath.Join(done, "artifacts", "dangling.log")); !os.IsNotExist(err) {
						t.Fatalf("tmp link survived as usable evidence: %v", err)
					}
				}
			}
		})
	}
}

// tasks_complete follows its handoff with the bulk warning `coop tasks done` prints when the
// completed folder holds a compiled program or more than 25 MB: for the assigned task, its
// already-done retry, and another task completed through the host's trusted path. The task still
// completes with every file in place. Evidence stays quiet, and so does scratch in tmp/, which the
// assigned move leaves for the host to remove. A name reaches the agent escaped.
func TestCompletionResultWarnsAboutBulk(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	const elf = "\x7fELF\x02\x01\x01\x00"
	const kept = "\nArchived tasks keep their files until someone deletes them.\nLeave the folder as it is and say what these files are in your final reply, so a person can decide what to keep."
	// write makes a file with an exact mode; a size past its content makes it sparse, since only the
	// apparent size counts.
	write := func(t *testing.T, path, content string, mode os.FileMode, size int64) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if err := os.Truncate(path, size); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  []string // what follows the handoff must hold each; none means nothing follows it
	}{{
		name:  "program",
		setup: func(t *testing.T, dir string) { write(t, filepath.Join(dir, "artifacts", "coop"), elf, 0o755, 0) },
		want:  []string{"\n\nThe task folder holds 1 compiled program.\nPrograms: artifacts/coop" + kept},
	}, {
		name: "oversized folder",
		setup: func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "artifacts", "trace.jsonl"), "", 0o644, 25_000_001)
		},
		want: []string{"\n\nThe task folder holds 25.0 MB.\nLargest: artifacts/trace.jsonl (25.0 MB), task.md (", kept},
	}, {
		name:  "escaped name",
		setup: func(t *testing.T, dir string) { write(t, filepath.Join(dir, "artifacts", "x\x1b[2Jy"), elf, 0o755, 0) },
		want:  []string{"\nPrograms: artifacts/x\\u001B[2Jy\n"},
	}, {
		name: "evidence and scratch",
		setup: func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "artifacts", "gate.log"), "ok\n", 0o644, 0)
			write(t, filepath.Join(dir, "artifacts", "repro.sh"), "#!/bin/sh\necho repro\n", 0o755, 0)
			write(t, filepath.Join(dir, "tmp", "coop"), elf, 0o755, 0)
			write(t, filepath.Join(dir, "tmp", "go.tar.gz"), "", 0o644, 30_000_000)
		},
	}} {
		for _, assigned := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/assigned=%v", tc.name, assigned), func(t *testing.T) {
				root := queue(t, map[string]string{"t1": tasks.StateInProgress})
				dir := filepath.Join(root, tasks.StateInProgress, "t1")
				finishChecklist(t, root, "t1")
				tc.setup(t, dir)
				var held []string
				if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
					if err == nil && !d.IsDir() {
						held = append(held, strings.TrimPrefix(path, dir+string(filepath.Separator)))
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				owner := ""
				if assigned {
					owner = "t1"
				}
				sess := newSession(t, newServer(t, root, owner))
				results := map[string]string{"t1 moved to 99_done/": sess.mustCall("tasks_complete", map[string]any{"id": "t1"})}
				if assigned {
					results["t1 is already done"] = sess.mustCall("tasks_complete", map[string]any{"id": "t1"})
				}
				for head, text := range results {
					after, ok := strings.CutPrefix(text, head+completionEvidenceHandoff)
					if !ok {
						t.Fatalf("the warning displaced the handoff: %q", text)
					}
					if len(tc.want) == 0 && after != "" {
						t.Errorf("%s: evidence and scratch drew a warning: %q", head, after)
					}
					for _, want := range tc.want {
						if !strings.Contains(after, want) {
							t.Errorf("%s: result lacks %q:\n%s", head, want, after)
						}
					}
					if strings.Contains(after, "\x1b") {
						t.Errorf("%s: a raw control character reached the agent: %q", head, after)
					}
				}
				// The warning is advice: the folder is done with every file it held, except the tmp/ the
				// host's trusted completion removes.
				for _, name := range held {
					_, err := os.Lstat(filepath.Join(root, tasks.StateDone, "t1", name))
					if gone := !assigned && strings.HasPrefix(name, "tmp"+string(filepath.Separator)); gone != os.IsNotExist(err) {
						t.Errorf("completion kept %s = %v, want removed %v", name, err, gone)
					}
				}
			})
		}
	}
}
