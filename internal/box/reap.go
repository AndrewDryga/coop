package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// reapMinimumAge is how long a temp entry must exist before it can be reaped
// even when nothing mounts it.
//
// Ownership alone is not enough: a box being launched right now has written its
// config and not yet started its container, so for a moment its files are
// genuinely unmounted. Deleting one then breaks the launch in a way that looks
// like a corrupt config. Age alone is not enough either, because a warm box can
// outlive any threshold. Together they are safe — old AND unmounted.
const reapMinimumAge = time.Hour

// tempPrefixes are the temp entries coop creates. Anything else in the temp
// directory belongs to someone else and is never touched.
var tempPrefixes = []string{
	"coop-mcp-", "coop-decoy-", "coop-decoy-dir-", "coop-githooks-",
	"coop-audit-trees-", "coop-skills-", "coop-test-task-leases-",
}

// mountLister is the part of the runtime reaping needs, named here so this can
// be tested without a container runtime.
type mountLister interface {
	MountSourcesByLabel(ctx context.Context, key, value string) (map[string]bool, error)
}

// ReapOrphanTempEntries removes coop temp files and directories that no box is
// using.
//
// These leak because cleanup is a deferred call, and a deferred call does not
// run when the process is killed — which is the normal way a box ends under
// supervision, a restart, or a machine going to sleep. The happy path already
// cleans up after itself; this is for every other path.
//
// It refuses to act rather than guess. If the runtime cannot be asked what is
// mounted, nothing is deleted: a transient failure to list containers must not
// read as permission to delete the files they are using.
func ReapOrphanTempEntries(
	ctx context.Context,
	lister mountLister,
	dir string,
	now time.Time,
) (int, error) {
	mounted, err := lister.MountSourcesByLabel(ctx, LabelKey, LabelBox)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var removed int
	var failures []error
	for _, entry := range entries {
		if !hasTempPrefix(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if mounted[path] {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < reapMinimumAge {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			failures = append(failures, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(failures...)
}

func hasTempPrefix(name string) bool {
	for _, prefix := range tempPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
