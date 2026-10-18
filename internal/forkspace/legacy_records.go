package forkspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/AndrewDryga/coop/internal/safefile"
)

// The bounds of one move. A real record is far smaller than legacyRecordLimit and a real folder far
// smaller than the budgets: a box could fill an older project's folder to stall the move or the
// disk, so past them the rest stays behind. A log copied across disks keeps its first
// legacyLogLimit bytes.
const (
	legacyRecordLimit = 1 << 20
	legacyLogLimit    = 16 << 20
)

// Variables only so a test can lower them.
var (
	legacyCopyBudget  int64 = 64 << 20
	legacyEntryBudget       = 20000
)

// moveLegacyEntry moves one anchor or log into the new folder; tests replace it to stand in for a
// project on another disk.
var moveLegacyEntry = renameAtNoReplace

// MoveLegacyForkRecords moves a project's fork records out of <repo>-forks/.coop, where builds up
// to v11.3.0 kept them, into StateDir. It runs the first time this build opens the project, and
// every box launch runs it before the box starts. A box could write the old place until then, so
// this is the one time Coop trusts it, and only as it is at that moment:
//   - The old folder is opened without following a link at any step, so a box cannot point the
//     move at another project's records.
//   - Its records are copied into a folder Coop makes, never renamed there, so a box that held a
//     folder or file of the old place open keeps writing the old copy. Generation anchors are moved
//     instead, since each must stay one file with its fork's workspace marker, and so are logs, which
//     only hold output. A link or other special file, or a file past legacyRecordLimit, stays behind.
//   - The new folder takes its name in one no-replace rename, then the old one is removed.
//
// A project with nothing to move gets its empty folder, so the old place is never read for it.
// One move runs at a time, under a lock in Coop's private forks folder, in a staging folder named
// for the project: one a crashed move left gives its anchors and logs back to the old folder first.
func MoveLegacyForkRecords(repo string) error {
	target := recordsPath(repo)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return err // moved already
	}
	root, err := PrivateStateRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	base, err := safefile.OpenRoot(root)
	if err != nil {
		return err
	}
	defer base.Close()
	forks, err := openStateChild(base, filepath.Join(root, "forks"), "forks", true)
	if err != nil {
		return err
	}
	defer forks.Close()
	unlock, err := lockLegacyMove(forks)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return err // another process moved them while this one waited
	}
	key := forkRecordsKey(repo)
	home, legacy, err := openLegacyRecords(repo)
	if err != nil {
		return err
	}
	if legacy == nil {
		if err := unix.Mkdirat(int(forks.Fd()), key, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return &os.PathError{Op: "mkdir", Path: target, Err: err}
		}
		return nil
	}
	defer home.Close()
	defer legacy.Close()
	staging := ".moving-" + key
	if err := restoreLeftoverMove(forks, staging, legacy); err != nil {
		return fmt.Errorf("give the fork records an interrupted move left in %s back to %s: %w",
			filepath.Join(forks.Name(), staging), legacy.Name(), err)
	}
	to, err := openStateChild(forks, filepath.Join(forks.Name(), staging), staging, true)
	if err != nil {
		return err
	}
	move := &legacyMove{}
	err = move.copy(legacy, to, 0)
	if err == nil {
		err = move.finish()
	}
	if err == nil {
		err = renameAtNoReplace(int(forks.Fd()), staging, int(forks.Fd()), key)
	}
	if err != nil {
		// Nothing half moved stays: the old folder keeps every record, and the next open starts over.
		move.undo()
		_ = to.Close()
		_ = os.RemoveAll(filepath.Join(forks.Name(), staging))
		move.close()
		return fmt.Errorf("move the fork records in %s to %s: %w", filepath.Join(Home(repo), ".coop"), target, err)
	}
	_ = to.Close()
	move.close()
	// The records are in place. Removing the old folder spares an older build stale records, unless
	// an anchor had to stay there: then the old folder is still whole for an older build.
	if !move.leftAnchor {
		_ = home.RemoveAll(".coop")
	}
	return nil
}

// lockLegacyMove takes the lock that lets one process at a time move fork records.
func lockLegacyMove(forks *os.File) (func(), error) {
	fd, err := unix.Openat(int(forks.Fd()), ".moving.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: filepath.Join(forks.Name(), ".moving.lock"), Err: err}
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return func() { _ = unix.Close(fd) }, nil
}

// openLegacyRecords opens <repo>-forks as a root and its .coop as a folder, following a link at
// neither, or returns no folder when there is nothing to move. Coop never made a link or other
// special file at either place, so one there is nothing to move: it is never followed, a project's
// box launches go on, and the fork commands that work in the forks folder refuse it themselves. A
// box that could plant one could as well have deleted the records behind it.
func openLegacyRecords(repo string) (*os.Root, *os.File, error) {
	if info, err := os.Lstat(Home(repo)); errors.Is(err, os.ErrNotExist) || err == nil && !realFolder(info) {
		return nil, nil, nil
	}
	home, err := openForkHome(repo)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	info, err := home.Lstat(".coop")
	if errors.Is(err, os.ErrNotExist) || err == nil && !realFolder(info) {
		_ = home.Close()
		return nil, nil, nil
	}
	var legacy *os.File
	if err == nil {
		legacy, err = home.OpenFile(".coop", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	}
	if err == nil {
		// The folder looked at, not one swapped in since.
		if opened, statErr := legacy.Stat(); statErr != nil || !os.SameFile(info, opened) {
			_ = legacy.Close()
			err = errors.Join(statErr, fmt.Errorf("the old fork records %s changed while Coop opened them", filepath.Join(Home(repo), ".coop")))
		}
	}
	if err != nil {
		_ = home.Close()
		return nil, nil, err
	}
	return home, legacy, nil
}

func realFolder(info os.FileInfo) bool { return info.IsDir() && info.Mode()&os.ModeSymlink == 0 }

// restoreLeftoverMove gives the anchors and logs a crashed move took out of the old folder back to
// it, then removes what that move left, so the move starts over from a whole old folder.
func restoreLeftoverMove(forks *os.File, staging string, legacy *os.File) error {
	fd, err := unix.Openat(int(forks.Fd()), staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	leftover := os.NewFile(uintptr(fd), filepath.Join(forks.Name(), staging))
	err = restoreLegacyEntries(leftover, legacy, 0)
	_ = leftover.Close()
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(forks.Name(), staging))
}

func restoreLegacyEntries(from, to *os.File, depth int) error {
	if depth > 8 {
		return nil
	}
	names, err := from.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(int(from.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch {
		case st.Mode&unix.S_IFMT == unix.S_IFDIR:
			child, err := openLegacyChild(from, name)
			if err != nil {
				return err
			}
			back, err := openLegacyChild(to, name)
			if err == nil {
				err = restoreLegacyEntries(child, back, depth+1)
				_ = back.Close()
			} else if errors.Is(err, unix.ENOENT) {
				err = nil
			}
			_ = child.Close()
			if err != nil {
				return err
			}
		case st.Mode&unix.S_IFMT == unix.S_IFREG && (strings.HasSuffix(name, ".anchor") || strings.HasSuffix(name, ".log")):
			if err := renameAtNoReplace(int(from.Fd()), name, int(to.Fd()), name); err != nil && !errors.Is(err, unix.EEXIST) {
				return err
			}
		}
	}
	return nil
}

func openLegacyChild(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

// legacyMove is one move's anchors and logs, moved once every record is copied, so a failure
// before then leaves the old folder whole.
type legacyMove struct {
	pending    []legacyEntry
	moved      []legacyEntry
	folders    []*os.File
	leftAnchor bool
	copied     int64
	entries    int
}

type legacyEntry struct {
	from, to *os.File
	name     string
	log      bool
}

// copy copies the records in from into to, making each folder anew, and notes the anchors and logs
// to move.
func (m *legacyMove) copy(from, to *os.File, depth int) error {
	if depth > 8 {
		return nil // deeper than any folder an older build made
	}
	names, err := from.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if m.entries++; m.entries > legacyEntryBudget {
			return nil
		}
		var st unix.Stat_t
		if err := unix.Fstatat(int(from.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch {
		case st.Mode&unix.S_IFMT == unix.S_IFDIR:
			err = m.copyFolder(from, to, name, depth)
		case st.Mode&unix.S_IFMT != unix.S_IFREG:
			// A link, pipe, socket or device: no older build made one.
		case strings.HasSuffix(name, ".anchor") || strings.HasSuffix(name, ".log"):
			m.pending = append(m.pending, legacyEntry{from: from, to: to, name: name, log: strings.HasSuffix(name, ".log")})
		case st.Size <= legacyRecordLimit && m.copied+st.Size <= legacyCopyBudget:
			m.copied += st.Size
			err = copyLegacyRecord(from, to, name, legacyRecordLimit)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(from.Name(), name), err)
		}
	}
	return nil
}

func (m *legacyMove) copyFolder(from, to *os.File, name string, depth int) error {
	fd, err := unix.Openat(int(from.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return nil // no longer a folder: not one an older build made
	}
	if err != nil {
		return err
	}
	child := os.NewFile(uintptr(fd), filepath.Join(from.Name(), name))
	m.folders = append(m.folders, child)
	made, err := openStateChild(to, filepath.Join(to.Name(), name), name, true)
	if err != nil {
		return err
	}
	m.folders = append(m.folders, made)
	return m.copy(child, made, depth+1)
}

// copyLegacyRecord copies the regular file name in from to a new owner-only file in to, reading at
// most limit bytes.
func copyLegacyRecord(from, to *os.File, name string, limit int64) error {
	in, err := unix.Openat(int(from.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil
	}
	if err != nil {
		return err
	}
	src := os.NewFile(uintptr(in), name)
	defer src.Close()
	if info, err := src.Stat(); err != nil || !info.Mode().IsRegular() {
		return err
	}
	out, err := unix.Openat(int(to.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	dst := os.NewFile(uintptr(out), name)
	_, err = io.Copy(dst, io.LimitReader(src, limit))
	return errors.Join(err, dst.Close())
}

// finish moves the anchors and logs. On another disk a log is copied instead, and an anchor stays
// behind: a hard link cannot cross disks, so that fork cannot move (requireOneDisk). What a move
// brought in is checked where no box can reach it: an entry swapped for a folder, link or pipe
// after it was looked at is removed.
func (m *legacyMove) finish() error {
	for _, entry := range m.pending {
		err := moveLegacyEntry(int(entry.from.Fd()), entry.name, int(entry.to.Fd()), entry.name)
		switch {
		case err == nil:
			var st unix.Stat_t
			if statErr := unix.Fstatat(int(entry.to.Fd()), entry.name, &st, unix.AT_SYMLINK_NOFOLLOW); statErr != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
				if removeErr := os.RemoveAll(filepath.Join(entry.to.Name(), entry.name)); removeErr != nil {
					return removeErr
				}
				continue
			}
			m.moved = append(m.moved, entry)
		case errors.Is(err, syscall.EXDEV) && entry.log:
			if m.copied+legacyLogLimit > legacyCopyBudget {
				continue
			}
			m.copied += legacyLogLimit
			if err := copyLegacyRecord(entry.from, entry.to, entry.name, legacyLogLimit); err != nil {
				return err
			}
		case errors.Is(err, syscall.EXDEV):
			m.leftAnchor = true
		default:
			return fmt.Errorf("%s: %w", filepath.Join(entry.from.Name(), entry.name), err)
		}
	}
	return nil
}

// undo puts back what finish moved, so the old folder is whole again.
func (m *legacyMove) undo() {
	for _, entry := range m.moved {
		_ = renameAtNoReplace(int(entry.to.Fd()), entry.name, int(entry.from.Fd()), entry.name)
	}
	m.moved = nil
}

func (m *legacyMove) close() {
	for _, folder := range m.folders {
		_ = folder.Close()
	}
}
