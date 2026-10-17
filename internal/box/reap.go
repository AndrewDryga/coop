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
	"github.com/AndrewDryga/coop/internal/runtime"
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

// tempPrefixes are the temp entries coop creates that no box mounts. Anything else in the temp
// directory belongs to someone else and is never touched. Neither are the box files earlier
// versions made there: none says which daemon its box ran on (see runDirPrefix).
var tempPrefixes = []string{
	"coop-audit-trees-", "coop-test-task-leases-",
	// Earlier versions kept an open run's MCP credential broker folder, with the run's real
	// credentials, here; a killed coop must not leave them behind. The helper that mounts it is not
	// a box — the hour of grace, and the container holding the bind-mounted file open until it
	// exits with its coop, are what kept a live run's folder safe.
	"coop-broker-",
	// A Compose command's private copy of registry auth, under the protected runfiles root. It
	// lives for that one command, well inside the hour of grace.
	"coop-compose-client-",
}

// runDirPrefix names the folder in the runfiles root where an ordinary run keeps every file its
// box mounts. Only the daemon that runs the box can say the folder is unused: any other daemon's
// listing never shows that box at all. So the launch records its daemon's ID in the folder
// (recordRunDaemon, recordDaemonAtStart), and a sweep judges the folder only by that daemon's
// listing. A folder that names another daemon, or none (made before the record existed, or where
// Docker printed no ID), is kept: removing a live box's files is worse than keeping a killed one's.
const runDirPrefix = "coop-run-"

// runDaemonFile, inside a run folder, holds the ID of the daemon the run's box was started on.
const runDaemonFile = "daemon"

// runfilesPrefixes adds what only the private runfiles root holds: each run's folder, and the empty
// folder a coop hands Docker as its client config for a whole run, never mounting it, so old and
// unmounted does not mean unused there. Removing a live one costs the run nothing, since Docker
// reads a missing one as empty, and in that owner-only folder no one else can recreate it. In a
// shared /tmp anyone could, with a Docker config of their own, so a temp directory sweep never
// looks for these, not even the ones earlier versions left there.
var runfilesPrefixes = slices.Concat(tempPrefixes, []string{"coop-docker-client-", runDirPrefix})

// mountLister is the part of the runtime reaping needs, named here so this can
// be tested without a container runtime.
type mountLister interface {
	// ComposeBinding names the daemon a runtime frozen by FreezeCompose lists, and only that one;
	// an unfrozen runtime names none.
	ComposeBinding() (endpoint, daemonID string)
	MountSourcesByLabel(ctx context.Context, key, value string) (map[string]bool, error)
}

// ErrMountsUnknown marks a reap that removed nothing because the runtime could not say what boxes
// mount. Unlike a failed removal, it is worth trying again soon.
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
	mounted, err := lister.MountSourcesByLabel(ctx, LabelKey, LabelBox)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrMountsUnknown, err)
	}
	return reapOrphanEntries(dir, tempPrefixes, "", mounted, now)
}

// ReapOrphanRunfiles sweeps Coop's private runfiles root by the same rules, Docker client folders
// included, and each run's folder only by the daemon it recorded. It locates the root itself, so
// these folders are only ever reaped from the validated owner-only one, and it creates nothing: a
// host that never staged a run has no root to sweep.
//
// A run folder is judged only by a frozen runtime, whose whole listing comes from the one daemon
// it names (runtime.FreezeCompose): the CLI otherwise follows whichever Docker context is current,
// so a `docker context use` mid-listing could pass one daemon's word off as another's.
func ReapOrphanRunfiles(ctx context.Context, lister mountLister, cfg *config.Config, now time.Time) (int, error) {
	root, exists, err := config.ExistingRunfiles(cfg)
	if err != nil || !exists {
		return 0, err
	}
	mounted, err := lister.MountSourcesByLabel(ctx, LabelKey, LabelBox)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrMountsUnknown, err)
	}
	_, daemon := lister.ComposeBinding()
	return reapOrphanEntries(root, runfilesPrefixes, daemon, mounted, now)
}

// recordRunDaemon names, inside a run's folder, the daemon its box is about to start on, or removes
// the record when daemon is "". It is best effort: a folder without the record is only ever kept,
// so a record it could not write is removed rather than left naming an earlier daemon.
func recordRunDaemon(runDir, daemon string) {
	if runDir == "" {
		return
	}
	path := filepath.Join(runDir, runDaemonFile)
	if daemon == "" || os.WriteFile(path, []byte(daemon+"\n"), 0o600) != nil {
		_ = os.Remove(path)
	}
}

// recordDaemonAtStart records the daemon again just before a box starts through the runtime CLI.
// The CLI starts the box on whichever daemon the current Docker context selects at that moment, not
// the one the launch probed before its setup, and a `docker context use` while the task channel,
// services or broker start would leave the folder naming a daemon that never sees this box: a sweep
// there would remove a live box's files. So the record is rewritten to the daemon that answers now.
// The one race left, a switch between this probe and the start, is the same whether the record
// changed or not; dropping a changed record instead would keep a killed run's credentials forever.
// A probe that names no daemon drops the record, so the folder is kept.
func recordDaemonAtStart(ctx context.Context, rt runtime.Runtime, runDir string) {
	if runDir == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	daemon, err := rt.DaemonID(ctx)
	if err != nil {
		daemon = ""
	}
	recordRunDaemon(runDir, daemon)
}

// startedOn reports whether the run folder at path recorded daemon as the one its box started on.
func startedOn(path, daemon string) bool {
	recorded, err := os.ReadFile(filepath.Join(path, runDaemonFile))
	return err == nil && daemon != "" && strings.TrimSpace(string(recorded)) == daemon
}

// reapOrphanEntries removes dir's old, unmounted entries with one of prefixes. mounted is what the
// boxes of daemon mount, so a run folder is removed only when its box started on that daemon.
func reapOrphanEntries(dir string, prefixes []string, daemon string, mounted map[string]bool, now time.Time) (int, error) {
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
		if strings.HasPrefix(name, runDirPrefix) && !startedOn(path, daemon) {
			continue
		}
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
