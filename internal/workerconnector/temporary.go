package workerconnector

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type sourceCredentialDirectory struct{}

// The host entrypoints supply this private directory, never the repository or
// controller job. Derived timeout contexts retain it; authenticated Git has no
// fallback to system temp, argv or environment credentials.
func withSourceCredentials(ctx context.Context, stateRoot string) (context.Context, error) {
	if !filepath.IsAbs(stateRoot) {
		return nil, errors.New("host Git requires a private state root")
	}
	if err := requirePrivateDirectory(stateRoot); err != nil {
		return nil, err
	}
	directory := filepath.Join(stateRoot, "host-git-tmp")
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := requirePrivateDirectory(directory); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, sourceCredentialDirectory{}, directory), nil
}

// ReclaimInterruptedTransfers runs only while the caller owns the session state
// lock, before starting service recovery or connector work. Only unpublished
// scratch is disposable; published sources and journaled response bodies remain.
// An orphan Git child may keep unlinked blocks open until it exits.
func ReclaimInterruptedTransfers(stateRoot string) error {
	if !filepath.IsAbs(stateRoot) {
		return errors.New("transfer cleanup requires an absolute state root")
	}
	if err := requirePrivateDirectory(stateRoot); err != nil {
		return err
	}
	for _, item := range []struct {
		directory, prefix string
		tree              bool
		suffix            func(string) bool
	}{
		{"job-sources", ".source-", true, scratchNumber},
		// A copy the worker's cleanup renamed to delete (sessionsvc.removeJobSource), its staging
		// key after the prefix: a delete cut short is finished here.
		{"job-sources", ".source-pruned-", true, stagingKey},
		{"host-git-tmp", "git-", false, scratchNumber},
		{filepath.Join("connector", "body-tmp"), "transfer-", false, scratchNumber},
	} {
		path := stateRoot
		missing := false
		for _, component := range strings.Split(item.directory, string(filepath.Separator)) {
			path = filepath.Join(path, component)
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				missing = true
				break
			}
			if err := requirePrivateDirectory(path); err != nil {
				return err
			}
		}
		if missing {
			continue
		}
		if err := reclaimTransferDirectory(path, item.prefix, item.tree, item.suffix); err != nil {
			return err
		}
	}
	return nil
}

// scratchNumber matches the random number os.MkdirTemp and os.CreateTemp put after a prefix.
func scratchNumber(suffix string) bool {
	number, err := strconv.ParseUint(suffix, 10, 32)
	return err == nil && strconv.FormatUint(number, 10) == suffix
}

// stagingKey matches a staged job source's name, a lowercase SHA-256 digest.
func stagingKey(suffix string) bool {
	decoded, err := hex.DecodeString(suffix)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == suffix
}

func reclaimTransferDirectory(path, prefix string, tree bool, valid func(string) bool) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			suffix, match := strings.CutPrefix(entry.Name(), prefix)
			if !match || !valid(suffix) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			stat, owned := info.Sys().(*syscall.Stat_t)
			if !owned || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 ||
				(tree && !info.IsDir()) || (!tree && !info.Mode().IsRegular()) {
				continue
			}
			target := filepath.Join(path, entry.Name())
			if tree {
				err = os.RemoveAll(target)
			} else {
				err = os.Remove(target)
			}
			if err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
