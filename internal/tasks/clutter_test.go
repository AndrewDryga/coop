package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// writeTaskBytes writes a task-folder file with an exact mode, creating its parents.
func writeTaskBytes(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	writeTaskFile(t, path, content)
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// taskTreeEntries lists every entry under dir, relative to it, without following symlinks.
func taskTreeEntries(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err == nil && rel != "." {
			names = append(names, rel)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// sparseFile makes a file of the given apparent size without writing its bytes.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	writeTaskFile(t, path, "")
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

// `coop tasks done` points out rebuildable bulk in the folder it archives — compiled programs and
// more than 25 MB in all, wherever in the folder they sit — and still completes the task, deleting
// nothing. Readable evidence (a log, an executable repro script), a non-executable ELF file,
// symlinks to a big file, a program or a folder of them outside the task, and a named pipe stay
// quiet; the pipe never stalls done.
func TestDoneWarnsAboutRebuildableBulk(t *testing.T) {
	const elf, macho = "\x7fELF\x02\x01\x01\x00", "\xcf\xfa\xed\xfe\x0c\x00\x00\x01"
	const advice = "\n  For a file a build can recreate, record its commit, sha256 and build command in log.md instead.\n"
	cases := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		want   []string // stderr lines that must appear; none means no warning at all
		absent []string
	}{{
		name: "compiled programs",
		setup: func(t *testing.T, dir string) {
			writeTaskBytes(t, filepath.Join(dir, "artifacts", "coop"), elf, 0o755)
			writeTaskBytes(t, filepath.Join(dir, "artifacts", "coop-linux"), elf, 0o755)
			writeTaskBytes(t, filepath.Join(dir, "artifacts", "lib", "coop.test"), elf, 0o700)
			writeTaskBytes(t, filepath.Join(dir, "coop-watch"), macho, 0o755)
			writeTaskBytes(t, filepath.Join(dir, "artifacts", "repro.sh"), "#!/bin/sh\necho repro\n", 0o755)
		},
		want: []string{
			"⚠ The task folder holds 4 compiled programs\n",
			"      Programs: artifacts/coop, artifacts/coop-linux, artifacts/lib/coop.test, and 1 more\n",
			"      Archived tasks keep their files until someone deletes them.\n",
			advice,
		},
		absent: []string{"repro.sh", "MB", "Largest:"},
	}, {
		name: "one file over 25 MB",
		setup: func(t *testing.T, dir string) {
			sparseFile(t, filepath.Join(dir, "artifacts", "trace.jsonl"), 25_000_001)
		},
		want:   []string{"⚠ The task folder holds 25.0 MB\n", "      Largest: artifacts/trace.jsonl (25.0 MB), ", advice},
		absent: []string{"Programs:", "including"},
	}, {
		// No single file comes near the limit; only their sum crosses it.
		name: "small files over 25 MB in all",
		setup: func(t *testing.T, dir string) {
			for i := range 6 {
				sparseFile(t, filepath.Join(dir, "artifacts", fmt.Sprintf("part-%d.bin", i)), 5_000_000)
			}
		},
		want: []string{
			"⚠ The task folder holds 30.0 MB\n",
			"      Largest: artifacts/part-0.bin (5.0 MB), artifacts/part-1.bin (5.0 MB), and artifacts/part-2.bin (5.0 MB)\n",
		},
		absent: []string{"Programs:"},
	}, {
		name: "both",
		setup: func(t *testing.T, dir string) {
			writeTaskBytes(t, filepath.Join(dir, "published", "runner"), macho, 0o755)
			sparseFile(t, filepath.Join(dir, "published", "runner.tar.gz"), 26_000_000)
		},
		want: []string{
			"⚠ The task folder holds 26.0 MB, including 1 compiled program\n",
			"      Programs: published/runner\n",
			"      Largest: published/runner.tar.gz (26.0 MB), ",
		},
	}, {
		// The walk never descends a symlink, so a linked toolchain is not counted.
		name: "symlink to a folder of programs",
		setup: func(t *testing.T, dir string) {
			outside := t.TempDir()
			writeTaskBytes(t, filepath.Join(outside, "bin", "go"), elf, 0o755)
			writeTaskBytes(t, filepath.Join(outside, "bin", "gofmt"), macho, 0o755)
			sparseFile(t, filepath.Join(outside, "pkg.tar.gz"), 30_000_000)
			writeTaskFile(t, filepath.Join(dir, "artifacts", "gate.log"), "ok\n")
			if err := os.Symlink(outside, filepath.Join(dir, "artifacts", "toolchain")); err != nil {
				t.Fatal(err)
			}
		},
	}, {
		name: "evidence only",
		setup: func(t *testing.T, dir string) {
			writeTaskFile(t, filepath.Join(dir, "artifacts", "gate.log"), "ok\n")
			writeTaskBytes(t, filepath.Join(dir, "artifacts", "repro.sh"), "#!/bin/sh\necho repro\n", 0o755)
			writeTaskBytes(t, filepath.Join(dir, "artifacts", "core"), elf, 0o644)
			outside := t.TempDir()
			sparseFile(t, filepath.Join(outside, "huge"), 30_000_000)
			writeTaskBytes(t, filepath.Join(outside, "program"), elf, 0o755)
			for link, target := range map[string]string{"huge-link": "huge", "program-link": "program"} {
				if err := os.Symlink(filepath.Join(outside, target), filepath.Join(dir, "artifacts", link)); err != nil {
					t.Fatal(err)
				}
			}
			if err := syscall.Mkfifo(filepath.Join(dir, "artifacts", "pipe"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			id := "2026-10-15-ship-it"
			dir := filepath.Join(root, StateInProgress, id)
			writeTaskFile(t, filepath.Join(dir, "task.md"), "# Ship it\n- [x] required checks passed\n")
			tc.setup(t, dir)
			kept := taskTreeEntries(t, dir)
			var code int
			var err error
			stderr := captureStderr(t, func() { code, err = tasksFolderMove(root, []string{id}, StateDone, "done", "done") })
			if code != 0 || err != nil {
				t.Fatalf("done = (%d, %v), want a completed task despite the warning\n%s", code, err, stderr)
			}
			if !strings.Contains(stderr, "✓ Completed task: Ship it") {
				t.Errorf("done lost its result line:\n%s", stderr)
			}
			if len(tc.want) == 0 && strings.Contains(stderr, "⚠") {
				t.Errorf("done warned about evidence:\n%s", stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("done output lacks %q:\n%s", want, stderr)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(stderr, absent) {
					t.Errorf("done output names %q:\n%s", absent, stderr)
				}
			}
			// The warning is advice: everything the folder held is still in the archived task.
			for _, name := range kept {
				if _, err := os.Lstat(filepath.Join(root, StateDone, id, name)); err != nil {
					t.Errorf("done lost %s: %v", name, err)
				}
			}
		})
	}

	// A retried done on an already-archived task (say, after a failed tmp cleanup) still warns.
	root := t.TempDir()
	id := "2026-10-15-already-done"
	writeTaskFile(t, filepath.Join(root, StateDone, id, "task.md"), "# Already done\n- [x] required checks passed\n")
	writeTaskBytes(t, filepath.Join(root, StateDone, id, "artifacts", "coop"), elf, 0o755)
	stderr := captureStderr(t, func() {
		if code, err := tasksFolderMove(root, []string{id}, StateDone, "done", "done"); code != 0 || err != nil {
			t.Fatalf("done on a done task = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(stderr, "Already done is already done.") || !strings.Contains(stderr, "⚠ The task folder holds 1 compiled program\n") {
		t.Errorf("an already-done retry must still warn about the program it archived:\n%s", stderr)
	}

	// The limit is the folder's total and it is exclusive: exactly 25,000,000 bytes stays quiet,
	// one byte more warns.
	const limit, taskMD = 25_000_000, "# At the limit\n"
	dir := t.TempDir()
	writeTaskFile(t, filepath.Join(dir, "task.md"), taskMD)
	sparseFile(t, filepath.Join(dir, "artifacts", "trace.jsonl"), limit-int64(len(taskMD)))
	if out := captureStderr(t, func() { warnTaskBulk(dir) }); out != "" {
		t.Errorf("a folder of exactly 25,000,000 bytes warned:\n%s", out)
	}
	sparseFile(t, filepath.Join(dir, "artifacts", "trace.jsonl"), limit-int64(len(taskMD))+1)
	if out := captureStderr(t, func() { warnTaskBulk(dir) }); !strings.Contains(out, "⚠ The task folder holds 25.0 MB\n") {
		t.Errorf("a folder one byte over 25,000,000 bytes did not warn:\n%s", out)
	}
}

// `coop tasks` / `coop tasks ls` end with a warning about anything at the queue root that is not
// part of the queue. The README, the state folders, the backlog and hidden entries (Coop's own
// .coop-queue.json, a tool's .conflux/, Finder's .DS_Store) never count, and the listing itself
// is unchanged on stdout.
func TestListWarnsAboutQueueRootStrays(t *testing.T) {
	root := t.TempDir()
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(root, "README.md"), "# Tasks\n")
	writeTaskFile(t, filepath.Join(root, QueueIdentityFile), "{}\n")
	writeTaskFile(t, filepath.Join(root, ".conflux", "config.json"), "{}\n")
	writeTaskFile(t, filepath.Join(root, ".DS_Store"), "")
	writeTaskFile(t, filepath.Join(root, StateBacklog, "2026-10-15-later", "task.md"), "# Later\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-10-15-fix-login", "task.md"), "# Fix login\n")

	list := func() (stdout, stderr string) {
		t.Helper()
		stderr = captureStderr(t, func() {
			stdout = captureStdout(t, func() {
				if code, err := tasksFolderList(root, false); code != 0 || err != nil {
					t.Fatalf("ls = (%d, %v)", code, err)
				}
			})
		})
		return stdout, stderr
	}
	clean, quiet := list()
	if quiet != "" {
		t.Fatalf("a queue root holding only its layout and hidden entries warned:\n%s", quiet)
	}

	writeTaskFile(t, filepath.Join(root, "release-notes.md"), "notes\n")
	stdout, stderr := list()
	if stdout != clean {
		t.Errorf("a stray changed the listing itself:\n%s", stdout)
	}
	for _, want := range []string{
		"⚠ " + root + " holds 1 item that is not part of the queue\n",
		"      release-notes.md\n",
		"  Move evidence into the task folder it belongs to, and anything else out of " + root + ".\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("listing lacks %q:\n%s", want, stderr)
		}
	}

	// Folders get a trailing slash, the list stops after three names, and an agent-chosen name
	// cannot send the terminal a control sequence.
	writeTaskFile(t, filepath.Join(root, "release-qualification", "goreleaser"), "")
	writeTaskFile(t, filepath.Join(root, "usage.log"), "")
	writeTaskFile(t, filepath.Join(root, "x\x1b[2Jy.txt"), "")
	_, stderr = list()
	if !strings.Contains(stderr, "holds 4 items that are not part of the queue\n") ||
		!strings.Contains(stderr, "      release-notes.md, release-qualification/, usage.log, and 1 more\n") {
		t.Errorf("listing did not summarize four strays:\n%s", stderr)
	}
	if err := os.Remove(filepath.Join(root, "release-notes.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "usage.log")); err != nil {
		t.Fatal(err)
	}
	_, stderr = list()
	if strings.Contains(stderr, "\x1b") || !strings.Contains(stderr, "      release-qualification/ and x\\u001B[2Jy.txt\n") {
		t.Errorf("a stray's control character must reach the terminal escaped, not raw: %q", stderr)
	}

	// A queue with no tasks yet still gets the warning, and so does an empty queue in the
	// multi-queue roll-up, which never reaches the single-queue listing.
	empty := t.TempDir()
	if err := ScaffoldStateDirs(filepath.Join(empty, ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(empty, ".agent", "tasks", "release.log"), "")
	stderr = captureStderr(t, func() {
		_ = captureStdout(t, func() {
			if code, err := tasksFolderList(filepath.Join(empty, ".agent", "tasks"), false); code != 0 || err != nil {
				t.Fatalf("ls on an empty queue = (%d, %v)", code, err)
			}
		})
	})
	if !strings.Contains(stderr, "      release.log\n") {
		t.Errorf("a queue with no tasks hid its stray:\n%s", stderr)
	}
	stderr = captureStderr(t, func() {
		_ = captureStdout(t, func() {
			if code, err := tasksListAll(empty, []string{".agent/tasks", "missing/.agent/tasks"}, nil); code != 0 || err != nil {
				t.Fatalf("ls across queues = (%d, %v)", code, err)
			}
		})
	})
	if !strings.Contains(stderr, "holds 1 item that is not part of the queue\n") || !strings.Contains(stderr, "      release.log\n") {
		t.Errorf("the roll-up skipped an empty queue's stray:\n%s", stderr)
	}
}
