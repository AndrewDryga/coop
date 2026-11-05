package box

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/processidentity"
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
// directory belongs to someone else and is never touched. That includes a run folder there: it
// does not say which daemon its box ran on, so no listing can prove it unused (see runDirPrefix).
var tempPrefixes = []string{
	"coop-audit-trees-",
	// What a killed test binary leaves: teststate's folder.
	"coop-test-state-",
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
// box mounts. Two things can still use one: the coop process that made it, and a container that
// run started. So the folder names both. Its owner record (recordRunOwner) is written first, and
// its daemon record (recordRunDaemon) before the run starts any container, because only the
// daemon that runs a box can say the box is gone: any other daemon's listing never shows it.
// runFolderUnused is the judgment; removing a live box's files is worse than keeping a killed one's.
const runDirPrefix = "coop-run-"

// runDaemonFile, inside a run folder, holds the ID of the daemon the run's box was started on.
const runDaemonFile = "daemon"

// runOwnerFile, inside a run folder, names the coop process that made it, as a supervisor label
// does (supervisorLabelValue), with a final newline that marks the record complete.
const runOwnerFile = "owner"

// rebootMargin is how long before this host booted a run folder must have last changed for the
// reboot alone to prove it unused. It covers a clock set forward after boot, which moves the boot
// time recorded by the kernel past folders a live process made before the change.
const rebootMargin = 24 * time.Hour

// runfilesPrefixes adds what only the private runfiles root holds: each run's folder, each editor
// session's tool handoff folder, and the empty folder a coop hands Docker as its client config for a
// whole run, never mounting it, so old and unmounted does not mean unused there. Removing a live one
// costs the run nothing, since Docker reads a missing one as empty, and in that owner-only folder no
// one else can recreate it. In a shared /tmp anyone could, with a Docker config of their own, so a
// temp directory sweep never looks for these, not even the ones earlier versions left there.
var runfilesPrefixes = slices.Concat(tempPrefixes, []string{"coop-docker-client-", runDirPrefix, mcpHandoffDirPrefix})

// ownedFolderPrefixes are the runfiles folders that name the process that made them
// (recordRunOwner): a live editor session keeps its handoff folder for days, so old and unmounted
// do not make one unused; runFolderUnused judges them.
var ownedFolderPrefixes = []string{runDirPrefix, mcpHandoffDirPrefix}

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
	return reapOrphanEntries(dir, tempPrefixes, runJudge{}, mounted, now)
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
	host, _ := thisHost()
	boot, _ := processidentity.BootTime()
	return reapOrphanRunfiles(ctx, lister, cfg, now, host, boot)
}

// reapOrphanRunfiles is ReapOrphanRunfiles for a coop on host, on a host that booted at boot (zero
// when unknown), so tests can judge owners and reboots this process cannot be.
func reapOrphanRunfiles(ctx context.Context, lister mountLister, cfg *config.Config, now time.Time, host string, boot time.Time) (int, error) {
	root, exists, err := config.ExistingRunfiles(cfg)
	if err != nil || !exists {
		return 0, err
	}
	mounted, err := lister.MountSourcesByLabel(ctx, LabelKey, LabelBox)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrMountsUnknown, err)
	}
	_, daemon := lister.ComposeBinding()
	return reapOrphanEntries(root, runfilesPrefixes, runJudge{daemon: daemon, host: host, boot: boot}, mounted, now)
}

// recordRunOwner names, inside a run folder it has just made, the coop process that made it, before
// anything else goes in: a run killed at any later moment leaves a folder a sweep can attribute.
// owner is "" when this host cannot name the process (supervisorLabelValue); the folder then has no
// record and is judged as an earlier Coop's were. A record cut short could read as another process
// that is gone, so only a complete one stays.
func recordRunOwner(runDir, owner string) {
	if owner == "" {
		return
	}
	path := filepath.Join(runDir, runOwnerFile)
	if os.WriteFile(path, []byte(owner+"\n"), 0o600) != nil {
		_ = os.Remove(path)
	}
}

// recordRunDaemon names, inside a run's folder, the daemon its box is about to start on; "" says
// the box may start on a daemon nobody could name, which no sweep can judge. Once written the record
// stays: a folder without one says its run never started a container, which lets a sweep remove it
// as soon as its owner is gone. So a record coop cannot write stops the launch instead.
func recordRunDaemon(runDir, daemon string) error {
	if runDir == "" {
		return nil
	}
	record := ""
	if daemon != "" {
		record = daemon + "\n"
	}
	if err := os.WriteFile(filepath.Join(runDir, runDaemonFile), []byte(record), 0o600); err != nil {
		return fmt.Errorf("record the Docker daemon in the run folder: %w", err)
	}
	return nil
}

// recordDaemonAtStart records the daemon again just before a box starts through the runtime CLI.
// The CLI starts the box on whichever daemon the current Docker context selects at that moment, not
// the one the launch probed before its setup, and a `docker context use` while the task channel,
// services or broker start would leave the folder naming a daemon that never sees this box: a sweep
// there would remove a live box's files. So the record is rewritten to the daemon that answers now.
// The one race left, a switch between this probe and the start, is the same whether the record
// changed or not; dropping a changed record instead would keep a killed run's credentials forever.
// A probe that names no daemon records none, so the folder is kept.
func recordDaemonAtStart(ctx context.Context, rt runtime.Runtime, runDir string) error {
	if runDir == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	daemon, err := rt.DaemonID(ctx)
	if err != nil {
		daemon = ""
	}
	return recordRunDaemon(runDir, daemon)
}

// runJudge holds what a pass knows when it judges a run folder: the daemon it pinned and listed
// ("" for none), the host field this coop writes, and when this host booted (zero when unknown).
type runJudge struct {
	daemon string
	host   string
	boot   time.Time
}

// runFolderUnused reports whether the run folder at path, last changed at changed, has records
// that prove nothing outside the caller's mount listing can still use it. The caller removes it only
// when it is also old and no listed box mounts it.
//
//   - It names its owner: that process is provably gone from this host (pid gone or reused), and
//     the run either started no container (no daemon record), started its box on the pinned daemon
//     (whose listing the caller checked), or ended before this host's last boot.
//   - It names no owner, as every folder an earlier Coop made: it names the pinned daemon, or it
//     last changed before this host's last boot. No process outlives a reboot, nor does a box on a
//     daemon that can mount this host's files.
//
// Everything else stays: a live owner, one this host cannot check, a record it cannot read, a box
// on another or an unnamed daemon, an owner-less folder from since the boot.
func (j runJudge) runFolderUnused(path string, changed time.Time) bool {
	beforeBoot := !j.boot.IsZero() && changed.Before(j.boot.Add(-rebootMargin))
	daemon, err := os.ReadFile(filepath.Join(path, runDaemonFile))
	started := !errors.Is(err, fs.ErrNotExist)
	onPinned := err == nil && j.daemon != "" && strings.TrimSpace(string(daemon)) == j.daemon
	owner, recorded, ok := runOwner(path)
	if !recorded {
		return onPinned || beforeBoot
	}
	if !ok || !supervisorProvablyDead(owner, j.host) {
		return false
	}
	return !started || onPinned || beforeBoot
}

// runOwner reads the owner record of the run folder at path. recorded is false when it has none;
// ok is true only for a complete record this version can read.
func runOwner(path string) (owner supervisorRef, recorded, ok bool) {
	record, err := os.ReadFile(filepath.Join(path, runOwnerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return supervisorRef{}, false, false
	}
	label, complete := strings.CutSuffix(string(record), "\n")
	owner, ok = parseSupervisorLabel(label)
	return owner, true, err == nil && complete && ok
}

// leftoverRunAge is how old a run folder must be before doctor counts it as left behind: older than
// any setup, and old enough for the daily cleanup to have had its chance.
const leftoverRunAge = 24 * time.Hour

// LeftoverRunFolders counts the run folders in Coop's runfiles root that are over a day old and whose
// owner is gone or was never recorded: what killed runs left that the daily cleanup has not removed,
// because it has not run since or cannot yet prove that no box uses them. It reads only the folders'
// own records, asks no runtime and creates nothing, so doctor can report it anywhere. root is ""
// when there is no runfiles root.
func LeftoverRunFolders(cfg *config.Config, now time.Time) (root string, leftovers int, err error) {
	root, exists, err := config.ExistingRunfiles(cfg)
	if err != nil || !exists {
		return "", 0, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return root, 0, err
	}
	host, _ := thisHost()
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), runDirPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < leftoverRunAge {
			continue
		}
		if owner, recorded, ok := runOwner(filepath.Join(root, entry.Name())); !recorded || ok && supervisorProvablyDead(owner, host) {
			leftovers++
		}
	}
	return root, leftovers, nil
}

// reapOrphanEntries removes dir's old, unmounted entries with one of prefixes. mounted is what the
// boxes of judge's daemon mount, and a run folder goes only when judge proves it unused.
func reapOrphanEntries(dir string, prefixes []string, judge runJudge, mounted map[string]bool, now time.Time) (int, error) {
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
		if slices.ContainsFunc(ownedFolderPrefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }) &&
			!judge.runFolderUnused(path, info.ModTime()) {
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
