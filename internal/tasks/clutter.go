package tasks

import (
	"cmp"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/ui"
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

// warnTaskBulk is done's heads-up when the folder it just archived holds a compiled program or more
// than bulkWarnBytes. It looks at the whole folder, not only artifacts/: agents also leave builds at
// the task's top level and in folders of their own naming.
func warnTaskBulk(taskDir string) {
	size, programs, largest := taskFolderBulk(taskDir)
	big := size > bulkWarnBytes
	if !big && len(programs) == 0 {
		return
	}
	held := ui.Count(len(programs), "compiled program")
	switch {
	case big && len(programs) > 0:
		held = ui.Bytes(uint64(size)) + ", including " + held
	case big:
		held = ui.Bytes(uint64(size))
	}
	var facts []string
	if len(programs) > 0 {
		facts = append(facts, "Programs: "+someNames(programs))
	}
	if big {
		files := make([]string, len(largest))
		for i, f := range largest {
			files[i] = fmt.Sprintf("%s (%s)", ui.SafeInline(f.name), ui.Bytes(uint64(f.size)))
		}
		facts = append(facts, "Largest: "+ui.List(files, "and"))
	}
	facts = append(facts, "Archived tasks keep their files until someone deletes them.")
	ui.Note("")
	ui.Warning("The task folder holds "+held, strings.Join(facts, "\n"),
		"For a file a build can recreate, record its commit, sha256 and build command in log.md instead.")
}

// taskFile is a regular file in a task folder, by its slash-separated path inside the folder.
type taskFile struct {
	name string
	size int64
}

// taskFolderBulk measures a task folder: the total size of its regular files, the executables among
// them that are compiled programs, and its namesShown largest files, biggest first. The walk stays
// inside the folder and never descends through a symlink. Anything it cannot read is skipped, since
// the result only decides whether to warn.
func taskFolderBulk(taskDir string) (size int64, programs []string, largest []taskFile) {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return 0, nil, nil
	}
	defer root.Close()
	_ = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		size += info.Size()
		largest = append(largest, taskFile{name, info.Size()})
		slices.SortStableFunc(largest, func(a, b taskFile) int { return cmp.Compare(b.size, a.size) })
		largest = largest[:min(len(largest), namesShown)]
		if info.Mode().Perm()&0o111 != 0 && compiledProgram(root, name, info) {
			programs = append(programs, name)
		}
		return nil
	})
	return size, programs, largest
}

// compiledProgram reports whether the regular file the walk listed as name starts with an ELF or
// Mach-O header: what Coop's Linux boxes and macOS hosts build. An executable script is evidence a
// person can read, so it does not count. The entry may have changed since the walk listed it, so
// the open must not wait on a pipe (O_NONBLOCK), and os.SameFile rejects anything but the listed
// file: os.Root follows a symlink that stays inside the folder even when asked not to.
func compiledProgram(root *os.Root, name string, listed os.FileInfo) bool {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil || !os.SameFile(listed, opened) {
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
