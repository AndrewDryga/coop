package workerconnector

import (
	"context"
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
	}{
		{"job-sources", ".source-", true},
		{"host-git-tmp", "git-", false},
		{filepath.Join("connector", "body-tmp"), "transfer-", false},
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
		if err := reclaimTransferDirectory(path, item.prefix, item.tree); err != nil {
			return err
		}
	}
	return nil
}

func reclaimTransferDirectory(path, prefix string, tree bool) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			suffix, match := strings.CutPrefix(entry.Name(), prefix)
			number, err := strconv.ParseUint(suffix, 10, 32)
			if !match || err != nil || strconv.FormatUint(number, 10) != suffix {
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
