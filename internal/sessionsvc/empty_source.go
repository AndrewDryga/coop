package sessionsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

const emptyJobCommit = "6b883aa2202644da23f8cae15f2d8a71404566a7"
const emptyJobCommitObject = "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
	"author Coop <coop@localhost> 0 +0000\ncommitter Coop <coop@localhost> 0 +0000\n\nCoop empty workspace\n"

// Repository-free normal jobs still need private forks for tools, semantic repair
// and review. This seed is never mounted in a model sandbox or fetched remotely.
func emptyJobRepository(ctx context.Context, stateRoot string) (string, error) {
	parent := filepath.Join(stateRoot, "job-sources")
	if err := os.Mkdir(parent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if info, err := os.Lstat(parent); err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("empty job source parent is not private")
	}
	key := sha256.Sum256([]byte("coop-empty-workspace-v1"))
	final := filepath.Join(parent, hex.EncodeToString(key[:]))
	repository := filepath.Join(final, "repository")
	if _, err := os.Lstat(final); err == nil {
		return repository, verifyEmptyJobRepository(ctx, final)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	temporary, err := os.MkdirTemp(parent, ".source-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	seed := filepath.Join(temporary, "repository")
	if err := os.Mkdir(seed, 0o700); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--template=", "--object-format=sha1", "--initial-branch=main"},
		{"config", "user.name", "Coop"}, {"config", "user.email", "coop@localhost"},
	} {
		if err := forkspace.GitRefCommand(ctx, seed, args...).Run(); err != nil {
			return "", fmt.Errorf("initialize empty job repository: %w", err)
		}
	}
	for _, object := range []struct{ kind, content string }{{"tree", ""}, {"commit", emptyJobCommitObject}} {
		cmd := forkspace.GitRefCommand(ctx, seed, "hash-object", "-w", "-t", object.kind, "--stdin")
		cmd.Stdin = strings.NewReader(object.content)
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("write empty job baseline: %w", err)
		}
	}
	if err := forkspace.GitRefCommand(ctx, seed, "update-ref", "refs/heads/main", emptyJobCommit).Run(); err != nil {
		return "", err
	}
	if err := verifyEmptyJobRepository(ctx, temporary); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, final); err != nil {
		// Concurrent first creates can publish the same immutable seed.
		if _, statErr := os.Lstat(final); statErr != nil {
			return "", err
		}
	}
	return repository, verifyEmptyJobRepository(ctx, final)
}

func verifyEmptyJobRepository(ctx context.Context, directory string) error {
	repository := filepath.Join(directory, "repository")
	for _, path := range []string{directory, repository, filepath.Join(repository, ".git")} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("empty job baseline is not a private Git repository")
		}
		if path != filepath.Join(repository, ".git") && info.Mode().Perm()&0o077 != 0 {
			return errors.New("empty job baseline is not private")
		}
	}
	head, err := sessionWorkspaceCommitContext(ctx, repository, "HEAD")
	if err != nil || head != emptyJobCommit {
		return errors.New("empty job baseline commit changed")
	}
	entries, err := os.ReadDir(repository)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
		return errors.New("empty job baseline working tree changed")
	}
	remote, truncated, err := runSessionWorkspaceGitWithEnvContext(ctx, repository, 1024, nil, "remote")
	if err != nil || truncated || len(remote) != 0 {
		return errors.New("empty job baseline cannot have a remote")
	}
	return nil
}
