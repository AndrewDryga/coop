package forkspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Gitlinks reads exact committed dependencies without entering their worktrees
// or interpreting .gitmodules. Ordinary tree entries are streamed, not retained.
func Gitlinks(ctx context.Context, repository, commit string) (map[string]string, error) {
	if !validPinnedCommit(commit) {
		return nil, errors.New("invalid gitlink tree commit")
	}
	command, err := GitCommandWithEnv(ctx, repository, []string{
		"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL=",
	}, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 8192), 16<<10)
	scanner.Split(splitGitRecord)
	links := make(map[string]string)
	for scanner.Scan() {
		header, path, ok := strings.Cut(scanner.Text(), "\t")
		if !ok {
			err = errors.New("malformed Git tree entry")
			break
		}
		if !strings.HasPrefix(header, "160000 commit ") {
			continue
		}
		id := strings.TrimPrefix(header, "160000 commit ")
		if !validPinnedCommit(id) || links[path] != "" || len(links) >= 1024 {
			err = errors.New("invalid or excessive gitlinks")
			break
		}
		links[path] = id
	}
	err = errors.Join(err, scanner.Err())
	if err != nil {
		_ = command.Process.Kill()
	}
	err = errors.Join(err, command.Wait(), ctx.Err())
	if err != nil {
		return nil, fmt.Errorf("read committed gitlinks: %w", err)
	}
	return links, nil
}

func splitGitRecord(data []byte, eof bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if eof && len(data) > 0 {
		return 0, nil, errors.New("unterminated Git record")
	}
	return 0, nil, nil
}

// SubmoduleDirectory refuses path redirection before a caller touches a child
// repository. Every component must already be a real checkout directory.
func SubmoduleDirectory(repository, path string) (string, error) {
	if !filepath.IsLocal(path) || strings.ContainsAny(path, "\\\x00\r\n") {
		return "", errors.New("invalid submodule path")
	}
	current := repository
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return "", errors.New("invalid submodule path")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("submodule directory is missing or redirected: %s", path)
		}
	}
	return current, nil
}
