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
)

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
		if filepath.Base(filepath.Dir(gitDir)) != "worktrees" ||
			filepath.Clean(filepath.Dir(filepath.Dir(gitDir))) != filepath.Clean(commonDir) {
			return "", nil, errors.New("project .git pointer does not name a linked-worktree admin directory")
		}
		backlink, present, err := readGitPointerFile(filepath.Join(gitDir, "gitdir"))
		if err != nil {
			return "", nil, fmt.Errorf("read linked-worktree backlink: %w", err)
		}
		if !present {
			return "", nil, errors.New("linked-worktree admin directory has no backlink")
		}
		backPath := strings.TrimSpace(string(backlink))
		if backPath == "" {
			return "", nil, errors.New("linked-worktree admin directory has an empty backlink")
		}
		if !filepath.IsAbs(backPath) {
			backPath = filepath.Join(gitDir, backPath)
		}
		if resolved, resolveErr := filepath.EvalSymlinks(backPath); resolveErr == nil {
			backPath = resolved
		}
		if filepath.Clean(backPath) != dotGit {
			return "", nil, errors.New("linked-worktree admin directory does not point back to this project")
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
