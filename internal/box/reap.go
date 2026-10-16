package box

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
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
	// One directory now contains every generated artifact for an ordinary run. Keeping the parent
	// together prevents same-UID boxes from enumerating one another's transient credentials.
	"coop-run-",
	// An open run's MCP credential broker keeps the run's real credentials here, so a killed
	// coop must not leave them behind. The mount check below lists a BOX's mounts, and the
	// helper is not a box — the hour of grace, and the container holding the bind-mounted file
	// open until it exits with its coop, are what keep a live run's directory safe.
	"coop-broker-",
	// A Compose command's private copy of registry auth, under the protected runfiles root. It
	// lives for that one command, well inside the hour of grace.
	"coop-compose-client-",
}

// runfilesPrefixes adds what only the private runfiles root holds: the empty folder a coop hands
// Docker as its client config for a whole run, never mounting it, so old and unmounted does not
// mean unused there. Removing a live one costs the run nothing, since Docker reads a missing one as
// empty, and in that owner-only folder no one else can recreate it. In a shared /tmp anyone could,
// with a Docker config of their own, so a temp directory sweep never looks for these, not even the
// ones earlier versions left there.
var runfilesPrefixes = slices.Concat(tempPrefixes, []string{"coop-docker-client-"})

// mountLister is the part of the runtime reaping needs, named here so this can
// be tested without a container runtime.
type mountLister interface {
	MountSourcesByLabel(ctx context.Context, key, value string) (map[string]bool, error)
}

// ErrMountsUnknown marks a reap that removed nothing because the runtime could
// not say what boxes mount. Unlike a failed removal, it is worth trying again soon.
var ErrMountsUnknown = errors.New("cannot tell what boxes mount")

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
	return reapOrphanEntries(ctx, lister, dir, tempPrefixes, now)
}

// ReapOrphanRunfiles sweeps Coop's private runfiles root by the same rules, Docker client folders
// included. It locates the root itself, so these folders are only ever reaped from the validated
// owner-only one, and it creates nothing: a host that never staged a run has no root to sweep.
func ReapOrphanRunfiles(ctx context.Context, lister mountLister, cfg *config.Config, now time.Time) (int, error) {
	root, exists, err := config.ExistingRunfiles(cfg)
	if err != nil || !exists {
		return 0, err
	}
	return reapOrphanEntries(ctx, lister, root, runfilesPrefixes, now)
}

func reapOrphanEntries(ctx context.Context, lister mountLister, dir string, prefixes []string, now time.Time) (int, error) {
	mounted, err := lister.MountSourcesByLabel(ctx, LabelKey, LabelBox)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrMountsUnknown, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var removed int
	var failures []error
	for _, entry := range entries {
		name := entry.Name()
		if !slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }) {
			continue
		}
		path := filepath.Join(dir, name)
		if tempEntryMounted(path, mounted) {
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

func tempEntryMounted(path string, mounted map[string]bool) bool {
	path = filepath.Clean(path)
	for source := range mounted {
		source = filepath.Clean(source)
		rel, err := filepath.Rel(path, source)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
