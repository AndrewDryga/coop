package forkspace

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/safefile"
)

// A project's fork records folder is named by a hash of its path (forkRecordsKey), and no record
// stores that path on its own, so a folder whose project was deleted stays forever unless a person
// removes it: testers delete scratch projects without coop fork rm. GoneForkRecords finds the
// folders that are safe to remove, and RemoveGoneForkRecords removes them on a person's word.

// emptyRecordsAge is how long an empty records folder must have been unchanged before it counts:
// a coop command may have just made it and be about to write its first record there.
const emptyRecordsAge = 24 * time.Hour

// goneRecordsReadLimit bounds what is read of one record while looking for its project's path.
const goneRecordsReadLimit = 1 << 20

// GoneRecords is what GoneForkRecords found: the records folders of projects that no longer exist,
// and the folders that hold no file at all.
type GoneRecords struct {
	Projects []GoneProject
	// Empty is the folders that hold no file at any depth but lock files no process holds, unchanged
	// for emptyRecordsAge. A records folder holds a record for every fork and running command, so one
	// without any protects nothing whichever project it belongs to.
	Empty []string
	// Kept counts the other folders: their project exists, or no record says which project it is.
	Kept int
}

// GoneProject is the records folder Key of the project at Path, which no longer exists, nor does
// its forks folder; Forks names the forks the records are for, when they say.
type GoneProject struct {
	Key, Path string
	Forks     []string
}

// GoneForkRecords reads every project's records folder under Coop's private state and says which
// may go. A folder counts only when it holds no file at all, or when one of its records names a
// path whose hash is the folder's name (so the folder is provably that project's) and neither that
// path nor its forks folder exists while the folder that held them does (projectGone). Any doubt
// keeps the records: a temporarily missing project must not lose its generations, rebase requests
// or land journals.
func GoneForkRecords(now time.Time) (GoneRecords, error) {
	var found GoneRecords
	forks, err := openForksFolder()
	if errors.Is(err, os.ErrNotExist) {
		return found, nil
	}
	if err != nil {
		return found, err
	}
	defer forks.Close()
	entries, err := fs.ReadDir(forks.FS(), ".")
	if err != nil {
		return found, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validRecordsKey(entry.Name()) {
			continue
		}
		// A folder that cannot be read through, often one a running command is changing, is kept.
		switch project, empty, err := goneRecordsFolder(forks, entry.Name(), now); {
		case err != nil:
			found.Kept++
		case empty:
			found.Empty = append(found.Empty, entry.Name())
		case project.Path != "":
			found.Projects = append(found.Projects, project)
		default:
			found.Kept++
		}
	}
	return found, nil
}

// RemoveGoneForkRecords removes the folders found, each only if it still counts when its turn
// comes: a project recreated at its path, or a record written into an empty folder, keeps it.
func RemoveGoneForkRecords(found GoneRecords, now time.Time) (removed int, err error) {
	forks, err := openForksFolder()
	if err != nil {
		return 0, err
	}
	defer forks.Close()
	keys := slices.Clone(found.Empty)
	for _, project := range found.Projects {
		keys = append(keys, project.Key)
	}
	for _, key := range keys {
		if !validRecordsKey(key) {
			return removed, fmt.Errorf("%q is not a fork records folder", key)
		}
		project, empty, err := goneRecordsFolder(forks, key, now)
		var done bool
		switch {
		case err != nil:
		case empty:
			done = removeEmptyFolder(forks, key)
		case project.Path != "":
			if done = !anyLockHeld(forks, key); done {
				if err := forks.RemoveAll(key); err != nil {
					return removed, err
				}
			}
		}
		if done {
			removed++
		}
	}
	return removed, nil
}

// removeEmptyFolder removes the records folder key, which holds no file but lock files, and the
// folders in it, deepest first, each with a plain rmdir: a file a coop command creates meanwhile
// makes the removal fail there and keeps it, where a re-check followed by RemoveAll would delete it.
// Each lock file is locked without waiting first, and all of them are unlinked while held, so a
// lock a process holds keeps the whole folder, and a coop command that opened one meanwhile finds it
// unlinked once it holds it and locks a new one (lockStateFile). It says whether the whole folder went.
func removeEmptyFolder(forks *os.Root, key string) bool {
	var dirs, locks []string
	err := fs.WalkDir(forks.FS(), key, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			dirs = append(dirs, path)
		case entry.Type().IsRegular() && strings.HasSuffix(path, ".lock"):
			locks = append(locks, path)
		default:
			return errors.New("not empty")
		}
		return nil
	})
	if err != nil {
		return false
	}
	var held []*os.File
	defer func() {
		for _, f := range held {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}
	}()
	for _, path := range locks {
		f, err := forks.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			return false
		}
		held = append(held, f)
	}
	for _, path := range locks {
		if err := forks.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if forks.Remove(dirs[i]) != nil {
			return false
		}
	}
	return true
}

// anyLockHeld says whether a process holds a lock file in the records folder key, or one cannot
// be checked: a loop whose repository was renamed while it ran still holds its fork's lock, and
// its records must stay. Each lock is tried without waiting and let go at once.
func anyLockHeld(forks *os.Root, key string) bool {
	folder, err := forks.OpenRoot(key)
	if err != nil {
		return true
	}
	defer folder.Close()
	held := false
	err = fs.WalkDir(folder.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".lock") {
			return nil
		}
		f, err := folder.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			held = true
			return fs.SkipAll
		}
		return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	})
	return held || err != nil
}

func openForksFolder() (*os.Root, error) {
	root, err := PrivateStateRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, "forks")
	dir, err := safefile.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	forks, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	want, err := dir.Stat()
	if err == nil {
		var got os.FileInfo
		if got, err = forks.Stat("."); err == nil && !os.SameFile(want, got) {
			err = fmt.Errorf("the fork records folder %s changed while Coop opened it", path)
		}
	}
	if err != nil {
		_ = forks.Close()
		return nil, err
	}
	return forks, nil
}

func validRecordsKey(name string) bool {
	return len(name) == 2*sha256.Size && strings.Trim(name, "0123456789abcdef") == ""
}

// recordPath is a quoted absolute path in a record, as JSON writes one.
var recordPath = regexp.MustCompile(`"(/[^"\\]+)"`)

// goneRecordsFolder says whether the records folder key holds no file and is old enough to count,
// or else which gone project it provably belongs to ("" when its project exists or is unknown).
func goneRecordsFolder(forks *os.Root, key string, now time.Time) (GoneProject, bool, error) {
	project := GoneProject{Key: key}
	folder, err := forks.OpenRoot(key)
	if err != nil {
		return project, false, err
	}
	defer folder.Close()
	var files []string
	newest := time.Time{}
	err = fs.WalkDir(folder.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return project, false, err
	}
	// Lock files are empty and name no project. A fork's removal unlinks its own (RemoveLockLocked),
	// but earlier builds left one behind for every removed fork, so a folder of lock files alone holds
	// no record.
	if !slices.ContainsFunc(files, func(file string) bool { return !strings.HasSuffix(file, ".lock") }) {
		return project, now.Sub(newest) >= emptyRecordsAge, nil
	}
	path := ""
	for _, file := range files {
		if strings.HasSuffix(file, ".lock") || path != "" {
			continue // lock files are empty
		}
		if path, err = recordProject(folder, file, key); err != nil {
			return project, false, err
		}
	}
	if path == "" || !projectGone(path) {
		return project, false, nil
	}
	project.Path = path
	for _, file := range files {
		if name, ok := strings.CutSuffix(file, ".generation.json"); ok && !strings.Contains(name, "/") {
			project.Forks = append(project.Forks, name)
		}
	}
	return project, false, nil
}

// recordProject is the project path a record names, proven by its hash being key, or "" when it
// names none. A fork's workspace, <project>-forks/<name>, names its project through its forks folder.
func recordProject(folder *os.Root, file, key string) (string, error) {
	f, err := folder.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, goneRecordsReadLimit))
	if err != nil {
		return "", err
	}
	for _, match := range recordPath.FindAllSubmatch(data, -1) {
		for path := filepath.Clean(string(match[1])); path != "/"; path = filepath.Dir(path) {
			if forkRecordsKeyOf(path) == key {
				return path, nil
			}
			if project, ok := strings.CutSuffix(path, Suffix); ok && forkRecordsKeyOf(project) == key {
				return project, nil
			}
		}
	}
	return "", nil
}

func forkRecordsKeyOf(path string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(path))) }

// projectGone says that neither the project at path nor its forks folder exists, while the folder
// that held them does: a real folder, not empty, and on the same disk as the folder above it. Every
// doubtful case keeps the records. A missing parent folder may be a disk that is not connected
// (/Volumes/<disk>), an empty one a mount point whose disk is gone, and one on another disk than
// its own parent is itself where a disk is mounted. So a scratch repository whose whole parent
// folder was deleted keeps its records too, and a moved repository loses them.
func projectGone(path string) bool {
	for _, p := range []string{path, Home(path)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	parent := filepath.Dir(path)
	if parent == "/" {
		return false
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() {
		return false
	}
	if same, err := sameDisk(parent, filepath.Dir(parent)); err != nil || !same {
		return false
	}
	dir, err := os.Open(parent)
	if err != nil {
		return false
	}
	defer dir.Close()
	names, _ := dir.Readdirnames(1)
	return len(names) > 0
}
