package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// A worker stages a private copy of every job source a create names (job-sources/<key>), and a
// session's fork lives inside it. Discarding the session removed the fork and kept the copy: on a
// Ryker tenant install (2026-10-13) each Work session staged all 17 repositories of its
// environment again, and the copies grew by about 3 GB an hour until the volume was hours from
// its stop line.
const (
	// A copy staged, reused or changed this recently may belong to a create whose session is not
	// recorded yet. Stage marks a copy it reuses.
	jobSourceInUseGrace = time.Hour
	// A task's next session runs its predecessor's frozen job, and Stage refuses to stage a frozen
	// source again once its branch has moved, so a task's own copy outlives its session this long.
	// A controller re-resolves any other session's sources before its next create.
	jobSourceTaskRetention = 30 * 24 * time.Hour
	// One pass removes at most this many copies, so cleanup never crowds out the work it frees
	// room for.
	jobSourcePrunePerPass = 16
	// A copy is renamed under the staging prefix before it is deleted: the inventory skips it,
	// and the next start finishes a delete that was cut short
	// (workerconnector.ReclaimInterruptedTransfers).
	jobSourcePrunePrefix = ".source-pruned-"
)

type stagedJobSource struct {
	key      string
	identity workerproto.RepositoryIdentity
	changed  time.Time
}

// pruneJobSources removes the staged copies no session can still use. A copy stays while a
// session that is not discarded names it, as its repository or a companion's; while a task's
// session updated within jobSourceTaskRetention names it as its repository; while it is the
// newest copy of its repository, which the next staging links from, or that repository's first
// download; while any fork but Coop's own records lives in it; and for jobSourceInUseGrace after
// it was staged, reused or changed. A copy Coop cannot read stays.
func (s *Service) pruneJobSources(ctx context.Context, sessions []session.Session, now time.Time) (int, []string) {
	root := filepath.Join(s.stateRoot, "job-sources")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, []string{fmt.Sprintf("read staged job sources: %v", err)}
	}
	needed, err := s.neededJobSources(ctx, sessions, now)
	if err != nil {
		return 0, []string{fmt.Sprintf("list the staged job sources sessions use: %v", err)}
	}
	var copies []stagedJobSource
	newest := map[workerproto.RepositoryIdentity]stagedJobSource{}
	for _, entry := range entries {
		if !entry.IsDir() || !validSessionDigest(entry.Name()) {
			continue
		}
		staged, ok := readStagedJobSource(filepath.Join(root, entry.Name()))
		if !ok {
			continue
		}
		copies = append(copies, staged)
		if current, seen := newest[staged.identity]; !seen || staged.changed.After(current.changed) {
			newest[staged.identity] = staged
		}
	}
	removed := 0
	var problems []string
	for _, staged := range copies {
		if removed >= jobSourcePrunePerPass || ctx.Err() != nil {
			break
		}
		repository := filepath.Join(root, staged.key, "repository")
		if needed[staged.key] || newest[staged.identity].key == staged.key ||
			staged.key == staged.identity.FirstDownloadKey() ||
			now.Sub(staged.changed) < jobSourceInUseGrace || holdsForks(repository) {
			continue
		}
		if err := s.removeJobSource(root, staged.key); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		removed++
	}
	return removed, problems
}

// neededJobSources is the set of staged copies, by key, that sessions may still use.
func (s *Service) neededJobSources(ctx context.Context, sessions []session.Session, now time.Time) (map[string]bool, error) {
	// A retired quarantined session left its runtime untouched, so its tombstone proves nothing.
	retired, err := s.store.RetiredQuarantinedSessions(ctx)
	if err != nil {
		return nil, err
	}
	runtimeMayHold := make(map[string]bool, len(retired))
	for _, id := range retired {
		runtimeMayHold[id] = true
	}
	needed := map[string]bool{}
	use := func(repository string) {
		if key, ok := stagedJobSourceKey(repository); ok {
			needed[key] = true
		}
	}
	for _, sess := range sessions {
		switch {
		case sess.State != session.SessionDiscarded || runtimeMayHold[sess.ID] || s.sessionQuarantined(sess.ID):
			use(sess.Repository)
			for _, companion := range sess.Companions {
				use(companion.Repository)
			}
		case sess.WorkspaceTask != nil && now.Sub(sess.UpdatedAt) < jobSourceTaskRetention:
			use(sess.Repository)
		}
	}
	return needed, nil
}

// stagedJobSourceKey reads the key out of a staged copy's repository path
// (job-sources/<key>/repository), so a state root reached through another name still matches.
func stagedJobSourceKey(repository string) (string, bool) {
	directory := filepath.Dir(repository)
	key := filepath.Base(directory)
	if filepath.Base(repository) != "repository" || filepath.Base(filepath.Dir(directory)) != "job-sources" ||
		!validSessionDigest(key) {
		return "", false
	}
	return key, true
}

// readStagedJobSource reads which repository a copy holds and when it was last staged, reused or
// changed. A copy whose receipt cannot be read is not reported, so it stays.
func readStagedJobSource(directory string) (stagedJobSource, bool) {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return stagedJobSource{}, false
	}
	receipt := filepath.Join(directory, "source.json")
	receiptInfo, err := os.Lstat(receipt)
	if err != nil || !receiptInfo.Mode().IsRegular() || receiptInfo.Size() > 256<<10 {
		return stagedJobSource{}, false
	}
	document, err := os.ReadFile(receipt)
	if err != nil {
		return stagedJobSource{}, false
	}
	var source workerproto.JobSource
	if json.Unmarshal(document, &source) != nil || source.RepositoryIdentity().Validate() != nil {
		return stagedJobSource{}, false
	}
	changed := info.ModTime()
	// A fork created or discarded changes the fork home, not the copy's directory.
	if forks, err := os.Lstat(forkspace.Home(filepath.Join(directory, "repository"))); err == nil &&
		forks.ModTime().After(changed) {
		changed = forks.ModTime()
	}
	return stagedJobSource{key: filepath.Base(directory), identity: source.RepositoryIdentity(), changed: changed}, true
}

// holdsForks reports whether a copy's fork home holds a fork, or a discard not yet finished.
// Coop's own fork records live in its private state; records an older build left in the home's
// .coop move first, and a .coop that had to stay (an anchor cannot cross disks) is no fork. One
// that cannot be read counts as holding them.
func holdsForks(repository string) bool {
	if err := forkspace.MoveLegacyForkRecords(repository); err != nil {
		return true
	}
	entries, err := os.ReadDir(forkspace.Home(repository))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	for _, entry := range entries {
		if entry.Name() != ".coop" {
			return true
		}
	}
	return false
}

// removeJobSource takes a copy out of the inventory in one rename, then deletes it. The storage
// measurement waits for it, so it never walks a tree mid-delete.
func (s *Service) removeJobSource(root, key string) error {
	s.storage.measureMu.Lock()
	defer s.storage.measureMu.Unlock()
	// The copy's fork records live in Coop's private state, keyed by the copy's own path, so they
	// go first: once the copy is retired, nothing names them.
	if err := os.RemoveAll(forkspace.StateDir(filepath.Join(root, key, "repository"))); err != nil {
		return fmt.Errorf("remove the fork records of staged job source %s: %w", key, err)
	}
	retired := filepath.Join(root, jobSourcePrunePrefix+key)
	if err := os.Rename(filepath.Join(root, key), retired); err != nil {
		return fmt.Errorf("retire staged job source %s: %w", key, err)
	}
	if err := os.RemoveAll(retired); err != nil {
		return fmt.Errorf("remove staged job source %s: %w", key, err)
	}
	return nil
}
