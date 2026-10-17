//go:build darwin || linux

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
)

const (
	localExcludeLimit = 1 << 20
	gitPointerLimit   = 64 << 10
	// A view refresh reads the repository's config whole on every host command; 4 MiB holds tens of
	// thousands of remotes and tracked branches.
	gitConfigLimit = 4 << 20
	// packed-refs, shallow and the index hold a line or an entry per ref, boundary commit or file,
	// so they grow with the repository; they are copied, never held in memory, and 1 GiB holds
	// millions of each.
	gitStoreFileLimit = 1 << 30
)

// gitFileTooLargeError refuses a repository file past limit, naming it and both sizes. Only a huge
// repository, or a damaged or replaced file, comes near these limits.
type gitFileTooLargeError struct {
	path        string
	size, limit int64
}

func gitFileTooLarge(path string, size, limit int64) error {
	return &gitFileTooLargeError{path: path, size: size, limit: limit}
}

func (e *gitFileTooLargeError) Error() string {
	return fmt.Sprintf("%s is %s, more than the %s Coop reads; if it should not be that large, it was damaged or replaced",
		e.path, byteSize(e.size), byteSize(e.limit))
}

// byteSize writes n in the largest of GiB, MiB and KiB it reaches, to one decimal unless whole.
func byteSize(n int64) string {
	for _, unit := range []struct {
		size int64
		name string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if n >= unit.size && n%unit.size == 0 {
			return fmt.Sprintf("%d %s", n/unit.size, unit.name)
		}
		if n >= unit.size {
			return fmt.Sprintf("%.1f %s", float64(n)/float64(unit.size), unit.name)
		}
	}
	return fmt.Sprintf("%d bytes", n)
}

var (
	syncLocalExcludeFile      = func(file *os.File) error { return file.Sync() }
	syncLocalExcludeDirectory = func(dir *os.File) error { return dir.Sync() }
	syncLocalGitDirectory     = func(dir *os.File) error { return dir.Sync() }
)

// ExcludeIfRepository is Exclude for project roots that Coop may run without Git. A hidden
// identity marker needs no Git rule there; every other lookup or write failure remains fatal.
func ExcludeIfRepository(ws, pattern string) error {
	commonDir, expected, err := safeExcludeTarget(ws)
	if errors.Is(err, errNoRepository) {
		return nil
	}
	if err != nil {
		return err
	}
	return excludeAt(commonDir, expected, pattern)
}

// Exclude appends one exact pattern to the repository's local info/exclude. The real common Git
// directory is used for plain clones and linked worktrees alike. Every write is relative to pinned
// no-follow directory handles so an agent-controlled symlink cannot redirect a host append.
func Exclude(ws, pattern string) error {
	commonDir, expected, err := safeExcludeTarget(ws)
	if err != nil {
		return err
	}
	return excludeAt(commonDir, expected, pattern)
}

// safeExcludeTarget accepts only the project's own real .git directory or a linked-worktree
// pointer whose admin directory links back to this exact .git file. A repository-controlled
// pointer to another checkout must not turn `coop approve` into a host write there.
func safeExcludeTarget(ws string) (string, os.FileInfo, error) {
	canonical, err := filepath.Abs(ws)
	if err == nil {
		canonical, err = filepath.EvalSymlinks(canonical)
	}
	if err != nil {
		return "", nil, err
	}
	dotGit := filepath.Join(canonical, ".git")
	dotInfo, err := os.Lstat(dotGit)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("%s: %w", ws, errNoRepository)
	}
	if err != nil {
		return "", nil, err
	}
	var gitDir, commonDir string
	switch {
	case dotInfo.IsDir() && dotInfo.Mode()&os.ModeSymlink == 0:
		gitDir = dotGit
		commonDir = dotGit
		if data, present, err := readGitPointerFile(filepath.Join(gitDir, "commondir")); err != nil {
			return "", nil, fmt.Errorf("read Git common-directory pointer: %w", err)
		} else if present {
			commonDir, err = resolveGitPointer(gitDir, data, "common-directory")
			if err != nil {
				return "", nil, err
			}
		}
		if filepath.Clean(commonDir) != dotGit {
			return "", nil, errors.New("project Git metadata does not resolve to its own .git directory")
		}
	case dotInfo.Mode().IsRegular():
		data, _, err := readGitPointerFile(dotGit)
		if err != nil {
			return "", nil, fmt.Errorf("read project .git pointer: %w", err)
		}
		line := strings.TrimSpace(string(data))
		if !strings.HasPrefix(line, "gitdir:") {
			return "", nil, errors.New("project .git entry is not a Git directory pointer")
		}
		gitDir, err = resolveGitPointer(canonical, []byte(strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))), "Git directory")
		if err != nil {
			return "", nil, err
		}
		commonDir = gitDir
		if data, present, err := readGitPointerFile(filepath.Join(gitDir, "commondir")); err != nil {
			return "", nil, fmt.Errorf("read linked-worktree common-directory pointer: %w", err)
		} else if present {
			commonDir, err = resolveGitPointer(gitDir, data, "common-directory")
			if err != nil {
				return "", nil, err
			}
		}
		readBacklink := func(path string) ([]byte, error) {
			data, present, err := readGitPointerFile(path)
			if err == nil && !present {
				err = os.ErrNotExist
			}
			return data, err
		}
		if err := checkLinkedWorktree(canonical, gitDir, commonDir, readBacklink); err != nil {
			return "", nil, err
		}
	default:
		return "", nil, errors.New("project .git entry is not an owner-controlled directory or linked-worktree pointer")
	}
	commonInfo, err := os.Lstat(commonDir)
	if err != nil {
		return "", nil, err
	}
	if !commonInfo.IsDir() || commonInfo.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("project common Git directory is not a real directory")
	}
	return commonDir, commonInfo, nil
}

// readGitPointerFile reads repository-controlled metadata without ever following the final name
// or blocking on a FIFO/device. The pre/post identity check closes the replace-between-check-and-
// open window; callers still validate where the pointer resolves before any host write.
func readGitPointerFile(path string) ([]byte, bool, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > gitPointerLimit {
		return nil, false, errors.New("git pointer is not a bounded regular file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		if err != nil {
			return nil, false, err
		}
		return nil, false, errors.New("git pointer changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, gitPointerLimit+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > gitPointerLimit {
		return nil, false, fmt.Errorf("git pointer exceeds %d bytes", gitPointerLimit)
	}
	return data, true, nil
}

// readGitFile reads a `.git` file, a linked worktree's or a submodule's pointer to its git dir,
// following a link as git does. A box can put anything there, so whatever the name leads to by
// the time it is open must be a regular file of at most gitPointerLimit bytes: the open never
// waits on a FIFO, and nothing is read without bound.
func readGitFile(path string) ([]byte, error) {
	data, _, err := readGitMetadata(path, gitPointerLimit)
	return data, err
}

// readGitMetadata is readGitFile for any repository file host code reads whole (a pointer, HEAD,
// the config) with the limit its purpose allows. It also returns the open file's status, so a
// caller comparing modification times compares the very bytes it read.
func readGitMetadata(path string, limit int64) ([]byte, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	// The size check refuses a huge sparse file before reading any of it; the bounded read still
	// stops a file that grows after the check.
	if info.Size() > limit {
		return nil, nil, gitFileTooLarge(path, info.Size(), limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, gitFileTooLarge(path, int64(len(data)), limit)
	}
	return data, info, nil
}

func resolveGitPointer(base string, data []byte, label string) (string, error) {
	target := strings.TrimSpace(string(data))
	if target == "" {
		return "", fmt.Errorf("%s pointer is empty", label)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve %s pointer: %w", label, err)
	}
	return filepath.Clean(resolved), nil
}

func excludeAt(commonDir string, expected os.FileInfo, pattern string) error {
	if pattern == "" || strings.ContainsAny(pattern, "\r\n") {
		return errors.New("invalid local Git exclusion")
	}
	common, err := os.OpenRoot(commonDir)
	if err != nil {
		return err
	}
	defer common.Close()
	if expected != nil {
		opened, openErr := common.Stat(".")
		named, nameErr := os.Lstat(commonDir)
		if openErr != nil || nameErr != nil || named.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(expected, opened) || !os.SameFile(opened, named) {
			return errors.Join(openErr, nameErr, errors.New("project Git directory changed while opening local excludes"))
		}
	}
	commonFile, err := common.Open(".")
	if err != nil {
		return err
	}
	defer commonFile.Close()
	infoFD, err := unix.Openat(int(commonFile.Fd()), "info",
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if errors.Is(err, syscall.ENOENT) {
		if err := unix.Mkdirat(int(commonFile.Fd()), "info", 0o755); err != nil && !errors.Is(err, syscall.EEXIST) {
			return err
		}
		// Confirm the info directory's name before using it as the durability boundary for the
		// exclude file it contains. Retrying this barrier is harmless when another caller won.
		if err := syncLocalGitDirectory(commonFile); err != nil {
			return err
		}
		infoFD, err = unix.Openat(int(commonFile.Fd()), "info",
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	}
	if err != nil {
		return fmt.Errorf("open local Git info directory: %w", err)
	}
	info := os.NewFile(uintptr(infoFD), "info")
	defer info.Close()
	excludeFD, err := unix.Openat(infoFD, "exclude",
		unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o644)
	if err != nil {
		return fmt.Errorf("open local Git exclude file: %w", err)
	}
	exclude := os.NewFile(uintptr(excludeFD), "exclude")
	defer exclude.Close()
	fileInfo, err := exclude.Stat()
	if err != nil {
		return err
	}
	stat, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok || !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 ||
		stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 || fileInfo.Size() < 0 || fileInfo.Size() > localExcludeLimit {
		return errors.New("local Git exclude is not a bounded owner-controlled regular file")
	}
	if _, err := exclude.Seek(0, 0); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(exclude, localExcludeLimit+1))
	if err != nil || len(data) > localExcludeLimit {
		return errors.Join(err, errors.New("local Git exclude exceeds its byte limit"))
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == pattern {
			// A prior append can be visible even though its file or directory fsync failed. An
			// idempotent retry must repeat both barriers before declaring the exclusion durable.
			return errors.Join(syncLocalExcludeFile(exclude), syncLocalExcludeDirectory(info))
		}
	}
	if _, err := exclude.WriteString("\n# coop: host state, never committed\n" + pattern + "\n"); err != nil {
		return err
	}
	if err := syncLocalExcludeFile(exclude); err != nil {
		return err
	}
	return syncLocalExcludeDirectory(info)
}
