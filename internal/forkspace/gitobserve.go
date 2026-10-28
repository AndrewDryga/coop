package forkspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/safefile"
)

// ObserveGit runs an observation against a fresh trusted metadata snapshot. Unlike
// operational GitCommand, it never reconciles a cached HEAD or refreshes the real
// index. Callers must use observation-only commands: the object store is shared.
func ObserveGit(ctx context.Context, repository string, args ...string) ([]byte, error) {
	cmd, cleanup, err := observationGitCommand(ctx, repository, args...)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	output, err := cmd.Output()
	return output, errors.Join(err, ctx.Err())
}

// ObserveCheckoutBlob streams Git's built-in checkout conversion from a fresh
// non-executing view. Drivers, includes and external attributes are unavailable;
// native EOL/ident/encoding conversion still matches the publication checkout.
func ObserveCheckoutBlob(ctx context.Context, repository, object, path string, destination io.Writer) error {
	if !ValidObjectID(object) {
		return errors.New("invalid checkout blob")
	}
	cmd, cleanup, err := observationGitCommandWithCheckout(ctx, repository, true, "cat-file", "--filters", "--path="+path, object)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdout = destination
	return errors.Join(cmd.Run(), ctx.Err())
}

func observationGitCommand(ctx context.Context, repository string, args ...string) (*exec.Cmd, func(), error) {
	return observationGitCommandWithCheckout(ctx, repository, false, args...)
}

func observationGitCommandWithCheckout(ctx context.Context, repository string, checkout bool, args ...string) (_ *exec.Cmd, _ func(), err error) {
	view, err := fsckGitView(ctx, repository)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(view.dir) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	if checkout {
		if err = includeTrustedCheckoutConfig(ctx, view); err != nil {
			return nil, nil, err
		}
	}
	root, err := safefile.OpenRoot(view.gitDir)
	if err != nil {
		return nil, nil, err
	}
	// The copy keeps git off the box-writable index, so a FIFO there cannot stall it; its limit
	// keeps a box from making Coop write a terabyte of holes out in full.
	err = copyGitMetadataFile(ctx, root, "index", filepath.Join(view.dir, "index"), gitStoreFileLimit)
	var tooLarge *gitFileTooLargeError
	if errors.As(err, &tooLarge) {
		// Index version 4 compresses the paths, which git documents as 30-50% smaller on large repositories.
		err = fmt.Errorf("%w; if it should, `git update-index --index-version 4` in the repository writes a smaller one", err)
	}
	err = errors.Join(err, root.Close())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	common, err := safefile.OpenRoot(view.commonDir)
	if err != nil {
		return nil, nil, err
	}
	exclude, readErr := safefile.ReadRegular(common, "info/exclude", 1<<20)
	closeErr := common.Close()
	if closeErr != nil || readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, nil, errors.Join(readErr, closeErr)
	}
	if readErr == nil {
		if err = os.Mkdir(filepath.Join(view.dir, "info"), 0o700); err != nil {
			return nil, nil, err
		}
		if err = os.WriteFile(filepath.Join(view.dir, "info", "exclude"), exclude, 0o600); err != nil {
			return nil, nil, err
		}
	}
	cmd := TrustedGitCommand(ctx, repository, view.envFrom(IndependentGitEnv()), args...)
	cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+filepath.Join(view.dir, "index"))
	return cmd, cleanup, nil
}

// ImportIsolatedObjects is the first publication effect. Only objects cross from
// retained custody; source refs and operational HEAD recovery do not participate.
func ImportIsolatedObjects(ctx context.Context, source, repository, commit string) error {
	if !ValidObjectID(commit) {
		return errors.New("invalid publication commit")
	}
	src, err := pinnedSourceView(ctx, source)
	if err != nil {
		return err
	}
	defer os.RemoveAll(src.dir)
	dst, err := fsckGitView(ctx, repository)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dst.dir)
	cmd := TrustedGitCommand(ctx, repository, dst.envFrom(pinnedTransferEnv()),
		"fetch", "--quiet", "--no-write-fetch-head", "--no-tags", "--recurse-submodules=no", "--", src.dir, commit)
	return errors.Join(cmd.Run(), ctx.Err())
}

// CheckoutIsolatedTree advances files/index before the recorded target ref. A
// failure can leave a partial checkout: callers must retain the forward journal,
// never reset, stash, delete index.lock, or treat this as atomic publication.
func CheckoutIsolatedTree(ctx context.Context, repository, before, publication string) error {
	if !ValidObjectID(before) || !ValidObjectID(publication) {
		return errors.New("invalid isolated publication target")
	}
	view, err := fsckGitView(ctx, repository)
	if err != nil {
		return err
	}
	defer os.RemoveAll(view.dir)
	if err := includeTrustedCheckoutConfig(ctx, view); err != nil {
		return err
	}
	// read-tree expects refreshed stat data. The caller already verifies all bytes
	// semantically; -q leaves unchanged hydrated LFS entries alone without filters.
	refresh := TrustedGitCommand(ctx, repository, view.envFrom(IndependentGitEnv()),
		"update-index", "-q", "--refresh", "--ignore-submodules")
	if output, err := refresh.CombinedOutput(); err != nil {
		if len(output) > 4096 {
			output = output[len(output)-4096:]
		}
		return errors.Join(fmt.Errorf("refresh isolated index: %w: %s", err, strings.TrimSpace(string(output))), ctx.Err())
	}
	cmd := TrustedGitCommand(ctx, repository, view.envFrom(IndependentGitEnv()),
		"read-tree", "--no-sparse-checkout", "--no-recurse-submodules", "-u", "-m", before, publication)
	output, runErr := cmd.CombinedOutput()
	if len(output) > 4096 {
		output = output[len(output)-4096:]
	}
	if runErr != nil {
		return errors.Join(fmt.Errorf("checkout isolated tree: %w: %s", runErr, strings.TrimSpace(string(output))), ctx.Err())
	}
	return ctx.Err()
}
