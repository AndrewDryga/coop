package forkspace

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// IndependentGitEnv retains ordinary host facilities (including signing agents),
// but no ambient Git configuration, filters, lazy fetching or index refresh.
func IndependentGitEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", "GIT_LFS_SKIP_SMUDGE=1",
		"GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
}

// CheckoutIndependent is only for a newly owned unpublished clone. Never use
// its reset on a user's checkout or an execution workspace containing new work.
func CheckoutIndependent(ctx context.Context, repository, commit, branch string) error {
	if !validPinnedCommit(commit) {
		return errors.New("invalid independent checkout commit")
	}
	args := []string{"update-ref", "--no-deref", "HEAD", commit}
	if branch != "" {
		ref := "refs/heads/" + branch
		command := GitRefCommand(ctx, repository, "update-ref", ref, commit)
		command.Env = IndependentGitEnv()
		if err := command.Run(); err != nil {
			return err
		}
		args = []string{"symbolic-ref", "HEAD", ref}
	}
	command := GitRefCommand(ctx, repository, args...)
	command.Env = IndependentGitEnv()
	if err := command.Run(); err != nil {
		return err
	}
	checkout, err := GitCommandWithEnv(ctx, repository, IndependentGitEnv(),
		"-c", "core.attributesFile="+os.DevNull, "reset", "--hard", "--quiet", commit)
	if err != nil {
		return err
	}
	return errors.Join(checkout.Run(), ctx.Err())
}

// MaterializeIndependentTree copies only exact committed, unchanged gitlinks
// from initialized local children of the trusted source. It never reads a URL
// from .gitmodules or copies a child's executable Git metadata into the box.
func MaterializeIndependentTree(ctx context.Context, source, workspace, sourceCommit, commit string, lfsSources ...string) error {
	remaining := 1024
	return materializeIndependentTree(ctx, source, workspace, sourceCommit, commit, 0, &remaining, lfsSources...)
}

func materializeIndependentTree(ctx context.Context, source, workspace, sourceCommit, commit string, depth int, remaining *int, lfsSources ...string) error {
	pinned, err := Gitlinks(ctx, source, sourceCommit)
	if err != nil {
		return err
	}
	links, err := Gitlinks(ctx, workspace, commit)
	if err != nil || !maps.Equal(pinned, links) {
		return errors.Join(errors.New("changed submodules require separately authorized publication"), err)
	}
	sourceModules, err := gitModulesEntry(ctx, source, sourceCommit)
	if err != nil {
		return err
	}
	modules, err := gitModulesEntry(ctx, workspace, commit)
	if err != nil || sourceModules != modules {
		return errors.Join(errors.New("changed .gitmodules requires separately authorized publication"), err)
	}
	if depth >= 16 && len(links) != 0 {
		return errors.New("isolated submodules exceed the supported nesting depth")
	}
	if len(links) > *remaining {
		return errors.New("isolated submodules exceed the supported total count")
	}
	*remaining -= len(links)
	if err := HydrateLFS(ctx, workspace, commit, append([]string{source}, lfsSources...)...); err != nil {
		return err
	}
	for _, path := range slices.Sorted(maps.Keys(links)) {
		childSource, err := SubmoduleDirectory(source, path)
		if err != nil {
			return fmt.Errorf("initialize trusted submodule %q before creating an isolated fork: %w", path, err)
		}
		// Legitimate source submodules commonly use gitfiles into the parent's
		// modules store. Only the new destination must own a real independent .git.
		if _, err := GitMetadataDirectories(childSource); err != nil {
			return fmt.Errorf("trusted submodule %q is not initialized: %w", path, err)
		}
		if err := QualifyDefaultLFSStorage(ctx, childSource); err != nil {
			return fmt.Errorf("trusted submodule %q: %w", path, err)
		}
		child, err := SubmoduleDirectory(workspace, path)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(child)
		if err != nil || len(entries) != 0 {
			return errors.Join(fmt.Errorf("refusing to replace nonempty submodule %q", path), err)
		}
		stage, err := os.MkdirTemp(filepath.Dir(child), ".coop-isolated-submodule-")
		if err != nil {
			return err
		}
		err = func() error {
			defer os.RemoveAll(stage)
			if err := GitClonePinnedContext(ctx, childSource, stage, links[path]); err != nil {
				return err
			}
			if err := PropagateGitIdentityContext(ctx, source, stage); err != nil {
				return err
			}
			removeOrigin := GitRefCommand(ctx, stage, "remote", "remove", "origin")
			removeOrigin.Env = IndependentGitEnv()
			if err := removeOrigin.Run(); err != nil {
				return err
			}
			if err := CheckoutIndependent(ctx, stage, links[path], ""); err != nil {
				return err
			}
			if err := materializeIndependentTree(ctx, childSource, stage, links[path], links[path], depth+1, remaining); err != nil {
				return err
			}
			if err := ValidateIndependentGit(stage); err != nil {
				return err
			}
			head, err := GitCommandWithEnv(ctx, stage, IndependentGitEnv(), "rev-parse", "--verify", "HEAD^{commit}")
			if err != nil {
				return err
			}
			output, err := head.Output()
			if err != nil || string(output) != links[path]+"\n" {
				return errors.Join(errors.New("isolated child HEAD changed before installation"), err)
			}
			index, err := GitCommandWithEnv(ctx, stage, IndependentGitEnv(), "diff-index", "--cached", "--quiet", "--no-ext-diff", "--no-textconv", links[path], "--")
			if err != nil {
				return err
			}
			if err := index.Run(); err != nil {
				return fmt.Errorf("isolated child index changed before installation: %w", err)
			}
			if err := VerifyLFS(ctx, stage, links[path]); err != nil {
				return err
			}
			// Darwin cannot rename over an empty directory. Remove refuses any
			// content that arrived after the empty-placeholder check above.
			if err := os.Remove(child); err != nil {
				return err
			}
			return os.Rename(stage, child)
		}()
		if err != nil {
			return fmt.Errorf("materialize isolated submodule %q: %w", path, err)
		}
	}
	return nil
}

func gitModulesEntry(ctx context.Context, repository, commit string) (string, error) {
	if !validPinnedCommit(commit) {
		return "", errors.New("invalid submodule metadata commit")
	}
	data, err := ObserveGit(ctx, repository, "ls-tree", "-z", commit, "--", ".gitmodules")
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}
	header, path, ok := strings.Cut(string(data), "\t")
	fields := strings.Fields(header)
	if !ok || path != ".gitmodules\x00" || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" || !validPinnedCommit(fields[2]) {
		return "", errors.New("unsupported .gitmodules tree entry")
	}
	return string(data), nil
}
