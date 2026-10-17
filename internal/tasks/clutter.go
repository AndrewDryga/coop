package tasks

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/ui"
	"golang.org/x/sys/unix"
)

// Agents, not Coop, fill the queue with bulk: compiled binaries kept as "evidence", release
// tarballs, toolchains, release notes left at the queue root. The archive keeps all of it until a
// person prunes it, so these warnings point it out when it happens. They never refuse or delete
// anything; what to keep is the owner's call. The rule: task-folders-keep-evidence-not-bulk.

// bulkWarnBytes is the task-folder size above which done warns: 25 MB, in the decimal units
// ui.Bytes prints. Logs, screenshots and repros fit well below it; a build or a toolchain does not.
const bulkWarnBytes = 25_000_000

// namesShown is how many names a warning lists: the first programs or strays, the largest files.
const namesShown = 3

// The measuring walk stops past either limit, and the folder then counts as bulk that could not be
// measured fully: a box can build a tree of any shape with a shell loop, and the in-box task tool
// measures it while the box's task channel waits. 10,000 empty folders, the slowest shape, take
// about half a second. Evidence comes nowhere near either limit: in Coop's own queue the fullest
// finished task holds about 1,400 entries, 10 folder levels at most.
const (
	walkMaxDepth   = 64     // folder levels below the task folder; also bounds the folders held open
	walkMaxEntries = 10_000 // files and folders; the warning prints it as "10,000"
)

// TaskBulkWarning is the heads-up about a task that was just completed in the queue at queueRoot: a
// headline and the facts behind it, one per line, when its folder holds a compiled program or more
// than bulkWarnBytes, or is too deep or too full to measure. ok is false otherwise, which is most of
// the time. It looks at the whole folder, not only artifacts/: agents also leave builds at the
// task's top level and in folders of their own naming. `coop tasks done` and the loop print it with
// WarnTaskBulk, a fork landing with WarnTaskBulkWithFolder; the in-box task tool returns it to the
// agent that completed the task, through TaskBulkWarningBelow.
func TaskBulkWarning(queueRoot, taskDir string) (headline string, facts []string, ok bool) {
	return TaskBulkWarningBelow(filepath.Dir(filepath.Dir(queueRoot)), taskDir)
}

// TaskBulkWarningBelow is TaskBulkWarning measured from top, a folder that holds the task folder.
// The in-box task tool passes the folder the box mounts, which the box cannot replace, so the walk
// down to the task never starts below a folder the box could swap for a link (taskFolderBulk).
func TaskBulkWarningBelow(top, taskDir string) (headline string, facts []string, ok bool) {
	m := taskFolderBulk(top, taskDir)
	big := m.size > bulkWarnBytes
	if !big && len(m.programs) == 0 && m.stopped == "" {
		return "", nil, false
	}
	if m.stopped != "" {
		facts = append(facts, m.stopped)
	}
	if len(m.programs) > 0 {
		facts = append(facts, "Programs: "+someNames(m.programs))
	}
	if big {
		files := make([]string, len(m.largest))
		for i, f := range m.largest {
			files[i] = fmt.Sprintf("%s (%s)", ui.SafeInline(f.name), ui.Bytes(uint64(f.size)))
		}
		facts = append(facts, "Largest: "+ui.List(files, "and"))
	}
	facts = append(facts, "Archived tasks keep their files until someone deletes them.")
	if m.stopped != "" {
		return "The task folder could not be measured fully", facts, true
	}
	held := ui.Count(len(m.programs), "compiled program")
	switch {
	case big && len(m.programs) > 0:
		held = ui.Bytes(uint64(m.size)) + ", including " + held
	case big:
		held = ui.Bytes(uint64(m.size))
	}
	return "The task folder holds " + held, facts, true
}

// WarnTaskBulk prints TaskBulkWarning under a completion a person reads, and reports whether it did.
func WarnTaskBulk(queueRoot, taskDir string) bool {
	return warnTaskBulk(queueRoot, taskDir, false)
}

// WarnTaskBulkWithFolder is WarnTaskBulk for output with no completion line that names the task:
// a fork landing prints none and can archive several tasks at once, so the task's folder is the
// warning's first fact.
func WarnTaskBulkWithFolder(queueRoot, taskDir string) bool {
	return warnTaskBulk(queueRoot, taskDir, true)
}

// warnTaskBulk prints TaskBulkWarning, led by the task's folder when withFolder is set. Agents name
// task folders too, so the folder is escaped like the names after it.
func warnTaskBulk(queueRoot, taskDir string, withFolder bool) bool {
	headline, facts, ok := TaskBulkWarning(queueRoot, taskDir)
	if !ok {
		return false
	}
	if withFolder {
		facts = slices.Insert(facts, 0, "Folder: "+ui.SafeInline(displayPath(taskDir)))
	}
	ui.Note("")
	ui.Warning(headline, strings.Join(facts, "\n"),
		"For a file a build can recreate, record its commit, sha256 and build command in log.md instead.")
	return true
}

// taskFile is a regular file in a task folder, by its slash-separated path inside the folder.
type taskFile struct {
	name string
	size int64
}

// bulkMeasure is what a task folder keeps once it is done: the total size of its regular files, the
// executables among them that are compiled programs, its namesShown largest files, biggest first,
// and, when the walk stopped at a limit, the fact that says which.
type bulkMeasure struct {
	size     int64
	programs []string
	largest  []taskFile
	entries  int
	stopped  string
}

// taskFolderBulk measures the task folder taskDir, which top holds. Its top-level tmp/ does not
// count, since done removes it; the in-box task tool looks before the host has. The walk opens each
// folder from top down to the task from its parent, without following a link or waiting on a FIFO
// (openBelow): a box that swaps .agent, a fork's assignment folder, the state folder or the task
// folder for a link to a host folder would otherwise learn, from the tool's answer, the names and
// sizes of the files in it. A folder on the way that is a link measures nothing. The in-box task
// tool starts at the folder the box mounts; `coop tasks done` and the loop start at the project
// folder holding the queue, and show the warning to a person rather than to the box.
func taskFolderBulk(top, taskDir string) bulkMeasure {
	var m bulkMeasure
	rel, err := filepath.Rel(top, taskDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return m
	}
	fd, err := unix.Open(top, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return m
	}
	dir := os.NewFile(uintptr(fd), top)
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		sub := openBelow(dir, name, unix.O_DIRECTORY)
		dir.Close()
		if sub == nil {
			return m
		}
		dir = sub
	}
	defer dir.Close()
	m.walk(dir, "", 1)
	return m
}

// walk measures the folder dir, at path prefix, whose entries sit level folders below the task
// folder. The box is still running while the in-box task tool measures, and can swap any entry the
// listing named, so every entry is opened as a folder first, from its parent (openBelow): one that
// is a FIFO or a link by then is skipped like one that always was, never waited on or followed.
// Opening each folder from its parent also keeps its cost flat at any depth; resolved from the
// top, a chain of 500 folders took 5 s. Anything the walk cannot read is skipped, since the result
// only decides whether to warn.
func (m *bulkMeasure) walk(dir *os.File, prefix string, level int) {
	for _, base := range m.list(dir, prefix) {
		name := prefix + base
		sub := openBelow(dir, base, unix.O_DIRECTORY)
		if sub == nil {
			m.measureFile(dir, base, name)
			continue
		}
		if level > walkMaxDepth {
			sub.Close()
			m.stopped = fmt.Sprintf("Its folders nest more than %d levels deep.", walkMaxDepth)
			return
		}
		m.walk(sub, name+"/", level+1)
		sub.Close()
		if m.stopped != "" {
			return
		}
	}
}

// list returns the names in dir, at path prefix, in order, and counts them against the walk's
// budget as it reads them: a box can put any number of entries in a folder, and every folder the
// walk is inside holds its names until it is done. It reads no more than the budget has left, plus
// one that shows the budget ran out and one for the top-level tmp/, which is skipped and not
// counted; past the budget it stops the walk and returns nothing.
func (m *bulkMeasure) list(dir *os.File, prefix string) []string {
	names, _ := dir.Readdirnames(walkMaxEntries - m.entries + 2)
	if prefix == "" {
		names = slices.DeleteFunc(names, func(name string) bool { return name == "tmp" })
	}
	if m.entries += len(names); m.entries > walkMaxEntries {
		m.stopped = "It holds more than 10,000 files and folders."
		return nil
	}
	slices.Sort(names)
	return names
}

// measureFile adds the entry base of dir, at path name, when it is a regular file, and notes it
// when it is a compiled program. Its size comes from the folder itself, never through a link.
func (m *bulkMeasure) measureFile(dir *os.File, base, name string) {
	var st unix.Stat_t
	if unix.Fstatat(int(dir.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return
	}
	m.size += st.Size
	m.largest = append(m.largest, taskFile{name, st.Size})
	slices.SortStableFunc(m.largest, func(a, b taskFile) int { return cmp.Compare(b.size, a.size) })
	m.largest = m.largest[:min(len(m.largest), namesShown)]
	if st.Mode&0o111 != 0 && compiledProgram(dir, base, st) {
		m.programs = append(m.programs, name)
	}
}

// openBelow opens the entry base of dir the way ownAgentFiles in internal/box does: from its
// parent, never through a link and never waiting on a FIFO. flags adds unix.O_DIRECTORY to open
// only a folder. It returns nil when the entry cannot be opened that way.
func openBelow(dir *os.File, base string, flags int) *os.File {
	fd, err := unix.Openat(int(dir.Fd()), base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|flags, 0)
	if err != nil {
		return nil
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), base))
}

// compiledProgram reports whether the regular file the walk measured as base in dir starts with
// an ELF or Mach-O header: what Coop's Linux boxes and macOS hosts build. An executable script is
// evidence a person can read, so it does not count. The entry may have changed since it was
// measured, so only a regular file with the measured inode is read: both come from the same folder
// handle, so the inode alone names it (identity-fences-compare-the-inode), and a FIFO that took the
// inode number back is never read, since its read would wait on its writer.
func compiledProgram(dir *os.File, base string, measured unix.Stat_t) bool {
	f := openBelow(dir, base, 0)
	if f == nil {
		return false
	}
	defer f.Close()
	var opened unix.Stat_t
	if unix.Fstat(int(f.Fd()), &opened) != nil || opened.Ino != measured.Ino || opened.Mode&unix.S_IFMT != unix.S_IFREG {
		return false
	}
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	switch string(magic[:]) {
	case "\x7fELF", // Linux
		"\xfe\xed\xfa\xce", "\xfe\xed\xfa\xcf", "\xce\xfa\xed\xfe", "\xcf\xfa\xed\xfe", // macOS, 32 or 64 bits, either byte order
		"\xca\xfe\xba\xbe", "\xca\xfe\xba\xbf": // a macOS universal binary
		return true
	}
	return false
}

// warnQueueRootStrays follows a listing with anything at the queue root that is not part of the
// queue. Nothing reads or removes such leftovers, so without a warning they pile up unseen.
func warnQueueRootStrays(root string) {
	strays := queueRootStrays(root)
	if len(strays) == 0 {
		return
	}
	verb := "are"
	if len(strays) == 1 {
		verb = "is"
	}
	where := displayPath(root)
	ui.Note("")
	ui.Warning(fmt.Sprintf("%s holds %s that %s not part of the queue", where, ui.Count(len(strays), "item"), verb),
		someNames(strays),
		"Move evidence into the task folder it belongs to, and anything else out of "+where+".")
}

// queueRootStrays names what sits at the queue root besides its layout (the README, the state
// folders and the backlog), folders with a trailing slash. Hidden entries never count: Coop keeps
// .coop-queue.json there, and tools keep their own (a .conflux/ folder, Finder's .DS_Store).
func queueRootStrays(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil // the listing has already read this tree; a warning is not worth a second error
	}
	var strays []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "README.md" || name == StateBacklog || slices.Contains(TaskStates, name) {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		strays = append(strays, name)
	}
	return strays
}

// someNames lists the first few names the way a sentence does, then how many more there are.
// Agents choose these names, so control characters are escaped before they reach the terminal.
func someNames(names []string) string {
	list := make([]string, 0, namesShown+1)
	for _, name := range names[:min(len(names), namesShown)] {
		list = append(list, ui.SafeInline(name))
	}
	if more := len(names) - len(list); more > 0 {
		list = append(list, fmt.Sprintf("%d more", more))
	}
	return ui.List(list, "and")
}
