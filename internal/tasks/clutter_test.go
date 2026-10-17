package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
	"golang.org/x/sys/unix"
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
	queue, dir := doneTaskDir(t)
	writeTaskFile(t, filepath.Join(dir, "task.md"), taskMD)
	sparseFile(t, filepath.Join(dir, "artifacts", "trace.jsonl"), limit-int64(len(taskMD)))
	if out := captureStderr(t, func() { WarnTaskBulk(queue, dir) }); out != "" {
		t.Errorf("a folder of exactly 25,000,000 bytes warned:\n%s", out)
	}
	sparseFile(t, filepath.Join(dir, "artifacts", "trace.jsonl"), limit-int64(len(taskMD))+1)
	if out := captureStderr(t, func() { WarnTaskBulk(queue, dir) }); !strings.Contains(out, "⚠ The task folder holds 25.0 MB\n") {
		t.Errorf("a folder one byte over 25,000,000 bytes did not warn:\n%s", out)
	}
}

// Done removes a task's tmp/, folder or file, so scratch there never counts, though the in-box task
// tool measures the folder before the host has removed it. A tmp folder further down is not that
// scratch and still counts.
func TestTaskBulkWarningSkipsScratch(t *testing.T) {
	const elf = "\x7fELF\x02\x01\x01\x00"
	queue, dir := doneTaskDir(t)
	writeTaskFile(t, filepath.Join(dir, "task.md"), "# Ship it\n")
	writeTaskBytes(t, filepath.Join(dir, "tmp", "coop"), elf, 0o755)
	sparseFile(t, filepath.Join(dir, "tmp", "go.tar.gz"), 30_000_000)
	if headline, facts, ok := TaskBulkWarning(queue, dir); ok {
		t.Errorf("scratch in tmp/ warned: %s %q", headline, facts)
	}
	fileQueue, file := doneTaskDir(t)
	sparseFile(t, filepath.Join(file, "tmp"), 30_000_000)
	if headline, facts, ok := TaskBulkWarning(fileQueue, file); ok {
		t.Errorf("a tmp file warned: %s %q", headline, facts)
	}

	writeTaskBytes(t, filepath.Join(dir, "artifacts", "tmp", "coop"), elf, 0o755)
	headline, facts, ok := TaskBulkWarning(queue, dir)
	if !ok || headline != "The task folder holds 1 compiled program" ||
		strings.Join(facts, "\n") != "Programs: artifacts/tmp/coop\nArchived tasks keep their files until someone deletes them." {
		t.Errorf("a program in artifacts/tmp/ = %v, %q, %q", ok, headline, facts)
	}
}

// A box can build a tree no evidence needs, and the in-box task tool measures the folder while the
// box's task channel waits, so the walk stops past 64 levels of folders or 10,000 entries and says
// it could not measure the folder fully, still naming what it found. Each folder is opened from its
// parent: opened from the top every time, a chain of a few hundred took seconds.
func TestTaskBulkWarningStopsAtItsLimits(t *testing.T) {
	const elf = "\x7fELF\x02\x01\x01\x00"
	chain := func(t *testing.T, dir string, depth int) {
		t.Helper()
		writeTaskFile(t, filepath.Join(dir, "task.md"), "# Ship it\n")
		if err := os.MkdirAll(filepath.Join(dir, strings.Repeat("d/", depth)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	deepQueue, deep := doneTaskDir(t)
	writeTaskBytes(t, filepath.Join(deep, "artifacts", "coop"), elf, 0o755)
	chain(t, deep, 300)
	start := time.Now()
	out := captureStderr(t, func() { WarnTaskBulk(deepQueue, deep) })
	if took := time.Since(start); took > time.Second {
		t.Errorf("measuring a chain of 300 folders took %v", took)
	}
	want := "⚠ The task folder could not be measured fully\n\n" +
		"      Its folders nest more than 64 levels deep.\n" +
		"      Programs: artifacts/coop\n" +
		"      Archived tasks keep their files until someone deletes them.\n\n" +
		"  For a file a build can recreate, record its commit, sha256 and build command in log.md instead.\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("a chain of 300 folders printed\n%s\nwant it to end with\n%s", out, want)
	}

	// A chain of exactly 64 folders is measured fully, and holds nothing to warn about.
	limitQueue, limit := doneTaskDir(t)
	chain(t, limit, 64)
	if headline, facts, ok := TaskBulkWarning(limitQueue, limit); ok {
		t.Errorf("64 levels of folders warned: %s %q", headline, facts)
	}

	// 10,000 files and folders are measured fully, tmp/ not counted; one more is not.
	fullQueue, full := doneTaskDir(t)
	for i := range walkMaxEntries {
		if err := os.WriteFile(filepath.Join(full, fmt.Sprintf("f%05d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTaskFile(t, filepath.Join(full, "tmp", "scratch.log"), "")
	if headline, facts, ok := TaskBulkWarning(fullQueue, full); ok {
		t.Errorf("10,000 entries warned: %s %q", headline, facts)
	}
	writeTaskFile(t, filepath.Join(full, "task.md"), "# Ship it\n")
	headline, facts, ok := TaskBulkWarning(fullQueue, full)
	if !ok || headline != "The task folder could not be measured fully" ||
		strings.Join(facts, "\n") != "It holds more than 10,000 files and folders.\nArchived tasks keep their files until someone deletes them." {
		t.Errorf("10,001 entries = %v, %q, %q", ok, headline, facts)
	}

	// However wide a folder is, the walk reads only the names its budget has left (plus two) and
	// counts them as it reads them, so the box can make it neither read nor hold a million names.
	f, err := os.Open(full)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	nearly := bulkMeasure{entries: walkMaxEntries - 5}
	if names := nearly.list(f, ""); names != nil || nearly.stopped == "" || nearly.entries > walkMaxEntries+2 {
		t.Errorf("with 5 entries of budget left, the walk counted %d names of a folder of 10,002 and returned %d (stopped %q)",
			nearly.entries-(walkMaxEntries-5), len(names), nearly.stopped)
	}
}

// The box is still running while the in-box task tool measures its folder, so an entry listed as a
// folder can be a FIFO or a link by the time the walk opens it. The walk opens every entry as a
// folder first, so entries that are a FIFO or links from the start take that same path: the walk
// returns at once and never follows a link, to a folder of programs outside or back to its own.
func TestBulkWalkNeverWaitsOnAFIFOOrFollowsALink(t *testing.T) {
	const elf = "\x7fELF\x02\x01\x01\x00"
	queue, dir := doneTaskDir(t)
	writeTaskFile(t, filepath.Join(dir, "task.md"), "# Ship it\n")
	writeTaskFile(t, filepath.Join(dir, "artifacts", "gate.log"), "ok\n")
	if err := syscall.Mkfifo(filepath.Join(dir, "artifacts", "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeTaskBytes(t, filepath.Join(outside, "bin", "go"), elf, 0o755)
	for name, target := range map[string]string{"toolchain": outside, "self": "."} {
		if err := os.Symlink(target, filepath.Join(dir, "artifacts", name)); err != nil {
			t.Fatal(err)
		}
	}
	measured := make(chan bulkMeasure, 1)
	go func() { measured <- taskFolderBulk(filepath.Dir(filepath.Dir(queue)), dir) }()
	select {
	case m := <-measured:
		if m.stopped != "" || len(m.programs) != 0 || m.entries != 6 {
			t.Errorf("the walk went past the FIFO and links it listed: %+v", m)
		}
	case <-time.After(wait.Deadline):
		t.Fatalf("measuring a folder holding a FIFO did not return within %v", wait.Deadline)
	}
}

// The box can also swap a folder above the task folder, .agent or a state folder, for a link to a
// host folder while the in-box task tool locates and then measures the task. The walk starts at
// the project folder, which the box cannot replace, and follows no link down to the task, so it
// measures nothing there rather than describe the host folder to the box.
func TestBulkWalkNeverFollowsALinkAboveTheTask(t *testing.T) {
	const elf = "\x7fELF\x02\x01\x01\x00"
	for _, swapped := range []string{".agent", filepath.Join(".agent", "tasks", StateDone)} {
		t.Run(swapped, func(t *testing.T) {
			queue, dir := doneTaskDir(t)
			project := filepath.Dir(filepath.Dir(queue))
			rel, err := filepath.Rel(project, dir)
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			writeTaskBytes(t, filepath.Join(outside, strings.TrimPrefix(rel, swapped+string(filepath.Separator)), "bin", "secret-tool"), elf, 0o755)
			if err := os.RemoveAll(filepath.Join(project, swapped)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(project, swapped)); err != nil {
				t.Fatal(err)
			}
			if headline, facts, ok := TaskBulkWarning(queue, dir); ok {
				t.Errorf("the walk followed %s to a folder outside the project: %s %q", swapped, headline, facts)
			}
		})
	}
}

// A fork's queue sits deeper in the workspace the box mounts, below folders the box can also swap,
// and the project folder of its queue is one of them. Measured from the workspace, as the in-box
// task tool measures (TaskBulkWarningBelow), a swapped folder there measures nothing, where the
// walk from the project folder followed the link and described another project's folder.
func TestBulkWalkFromTheWorkspaceNeverFollowsALink(t *testing.T) {
	const elf = "\x7fELF\x02\x01\x01\x00"
	generation := filepath.Join(".coop", "task-executions", "generation")
	for _, swapped := range []string{generation, ".coop"} {
		t.Run(swapped, func(t *testing.T) {
			workspace := t.TempDir()
			queue := filepath.Join(workspace, generation, "assignment", "tasks")
			dir := filepath.Join(queue, StateDone, "2026-10-16-ship-it")
			writeTaskFile(t, filepath.Join(dir, "task.md"), "# Ship it\n")
			below, err := filepath.Rel(filepath.Join(workspace, swapped), dir)
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "other", ".agent")
			writeTaskBytes(t, filepath.Join(outside, below, "bin", "secret-tool"), elf, 0o755)
			if err := os.Rename(filepath.Join(workspace, swapped), filepath.Join(workspace, swapped)+".kept"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(workspace, swapped)); err != nil {
				t.Fatal(err)
			}
			if _, _, ok := TaskBulkWarning(queue, dir); !ok {
				t.Fatal("from the project folder the walk did not reach the other project, so the swap proves nothing")
			}
			if headline, facts, ok := TaskBulkWarningBelow(workspace, dir); ok {
				t.Errorf("the walk followed %s to another project: %s %q", swapped, headline, facts)
			}
		})
	}
}

// compiledProgram reads only the file the walk measured: an entry swapped since, for another
// program or a FIFO, is never read, and a FIFO never makes it wait, even one with a writer.
func TestCompiledProgramReadsOnlyTheMeasuredFile(t *testing.T) {
	const elf = "\x7fELF\x02\x01\x01\x00"
	dir := t.TempDir()
	writeTaskBytes(t, filepath.Join(dir, "measured"), elf, 0o755)
	writeTaskBytes(t, filepath.Join(dir, "swapped"), elf, 0o755)
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stat := func(name string) unix.Stat_t {
		var st unix.Stat_t
		if err := unix.Fstatat(int(f.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if !compiledProgram(f, "measured", stat("measured")) {
		t.Fatal("the measured program was not recognized")
	}
	if compiledProgram(f, "swapped", stat("measured")) {
		t.Error("a program other than the measured file was read")
	}
	// A silent writer holds the FIFO open, so a read of it would wait for data that never comes.
	writer, err := os.OpenFile(filepath.Join(dir, "fifo"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	read := make(chan bool, 1)
	go func() { read <- compiledProgram(f, "fifo", stat("fifo")) }()
	select {
	case got := <-read:
		if got {
			t.Error("a FIFO read as a compiled program")
		}
	case <-time.After(wait.Deadline):
		t.Fatalf("reading a FIFO did not return within %v", wait.Deadline)
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

// doneTaskDir makes an empty finished-task folder in a fresh queue, as the measuring walk expects
// to find one, and returns the queue root and the folder.
func doneTaskDir(t *testing.T) (string, string) {
	t.Helper()
	queue := filepath.Join(t.TempDir(), ".agent", "tasks")
	dir := filepath.Join(queue, StateDone, "2026-10-16-ship-it")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return queue, dir
}
