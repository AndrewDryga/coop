package forkctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
)

const (
	landIntentVersion = 1
	landIntentLimit   = 8 << 20

	landPreparing = "preparing"
	landRebased   = "rebased"
	landRestoring = "restoring"
	landApproved  = "approved"
	landLanded    = "landed"
)

type landIntent struct {
	Version      int                 `json:"version"`
	Candidate    tasks.ForkCandidate `json:"candidate"`
	ParentBefore string              `json:"parent_before"`
	RebasedHead  string              `json:"rebased_head,omitempty"`
	RebasedTree  string              `json:"rebased_tree,omitempty"`
	Phase        string              `json:"phase"`
	CreatedAt    time.Time           `json:"created_at"`
}

func landIntentPath(repo string, identity forkspace.Identity) string {
	return forkspace.LandIntentPath(repo, identity)
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func validateLandIntent(intent landIntent) error {
	if intent.Version != landIntentVersion || !validObjectID(intent.ParentBefore) || intent.CreatedAt.IsZero() {
		return errors.New("invalid fork land intent")
	}
	if err := tasks.ValidateForkCandidateRecord(intent.Candidate); err != nil {
		return fmt.Errorf("invalid candidate in fork land intent: %w", err)
	}
	switch intent.Phase {
	case landPreparing:
		if intent.RebasedHead != "" || intent.RebasedTree != "" {
			return errors.New("preparing land intent already has rebased identity")
		}
	case landRebased, landRestoring, landApproved, landLanded:
		if !validObjectID(intent.RebasedHead) || !validObjectID(intent.RebasedTree) {
			return errors.New("fork land intent is missing rebased identity")
		}
	default:
		return errors.New("invalid fork land intent phase")
	}
	return nil
}

func readLandIntent(repo string, identity forkspace.Identity) (landIntent, bool, error) {
	path := landIntentPath(repo, identity)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return landIntent{}, false, nil
	}
	if err != nil {
		return landIntent{}, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		info.Size() < 0 || info.Size() > landIntentLimit {
		return landIntent{}, false, errors.New("fork land intent is not a bounded single-link regular file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return landIntent{}, false, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		if err != nil {
			return landIntent{}, false, err
		}
		return landIntent{}, false, errors.New("fork land intent changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, landIntentLimit+1))
	if err != nil || len(data) > landIntentLimit {
		if err != nil {
			return landIntent{}, false, err
		}
		return landIntent{}, false, errors.New("fork land intent exceeds its size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var intent landIntent
	if err := dec.Decode(&intent); err != nil {
		return landIntent{}, false, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return landIntent{}, false, errors.New("fork land intent contains multiple JSON values")
		}
		return landIntent{}, false, err
	}
	if err := validateLandIntent(intent); err != nil {
		return landIntent{}, false, err
	}
	if intent.Candidate.Fork != identity {
		return landIntent{}, false, errors.New("fork land intent belongs to another generation")
	}
	return intent, true, nil
}

// ForkHasPendingLand lets fresh/rm/session teardown refuse an exact generation whose Git/task
// transaction must be replayed by merge rather than discarded.
func ForkHasPendingLand(repo string, identity forkspace.Identity) (bool, error) {
	_, ok, err := readLandIntent(repo, identity)
	return ok, err
}

func writeLandIntent(repo string, intent landIntent) error {
	if err := validateLandIntent(intent); err != nil {
		return err
	}
	if err := forkspace.EnsureStateDir(repo); err != nil {
		return err
	}
	body, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(forkspace.StateDir(repo), "."+intent.Candidate.Fork.Name+".land-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, landIntentPath(repo, intent.Candidate.Fork))
}

func removeLandIntent(repo string, intent landIntent) error {
	current, ok, err := readLandIntent(repo, intent.Candidate.Fork)
	if err != nil || !ok {
		return err
	}
	if current.Candidate.ID != intent.Candidate.ID || current.ParentBefore != intent.ParentBefore {
		return errors.New("fork land intent changed before removal")
	}
	if err := os.Remove(landIntentPath(repo, intent.Candidate.Fork)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func restoreCandidateWorkspace(repo, ws, name string, candidate tasks.ForkCandidate) error {
	if err := recoverInterruptedRebase(repo, ws, name); err != nil {
		return err
	}
	// `checkout -B` split in two: HEAD is rewritten on the real git dir (a view would keep that to
	// itself), then the branch, index and files reset under the view, where the repository's
	// config can name no filter for the checkout to run.
	if err := forkspace.GitRefCommand(context.Background(), ws, "update-ref", "refs/heads/"+name, candidate.Head).Run(); err != nil {
		return fmt.Errorf("%s: restore reviewed candidate after interrupted land: %w", name, err)
	}
	if err := forkspace.GitSwitchBranch(context.Background(), ws, name); err != nil {
		return fmt.Errorf("%s: restore reviewed candidate after interrupted land: %w", name, err)
	}
	return nil
}

func resetLandAfterParentAdvance(repo, ws, name string, intent landIntent, parentHead string) (landIntent, error) {
	if !validObjectID(parentHead) {
		return intent, errors.New("parent has no exact HEAD for land retry")
	}
	if err := restoreCandidateWorkspace(repo, ws, name, intent.Candidate); err != nil {
		return intent, err
	}
	intent.ParentBefore = parentHead
	intent.RebasedHead = ""
	intent.RebasedTree = ""
	intent.Phase = landPreparing
	if err := writeLandIntent(repo, intent); err != nil {
		return intent, err
	}
	return intent, nil
}

func (c *Control) finishRejectedTaskLand(repo, ws, name string, intent landIntent) (landIntent, bool, error) {
	if err := restoreCandidateWorkspace(repo, ws, name, intent.Candidate); err != nil {
		return intent, false, errors.Join(errors.New("merge gate failed"), err)
	}
	if c.afterLandCandidateRestore != nil {
		if err := c.afterLandCandidateRestore(); err != nil {
			return intent, false, err
		}
	}
	if err := removeLandIntent(repo, intent); err != nil {
		return intent, false, err
	}
	return intent, false, fmt.Errorf("%s: gate failed on the rebased fork — parent untouched; candidate restored", name)
}

func (c *Control) advanceTaskLand(repo, ws, name, img string, intent landIntent) (landIntent, bool, error) {
	if intent.Phase == landRestoring {
		return c.finishRejectedTaskLand(repo, ws, name, intent)
	}
	if intent.Phase == landPreparing {
		parentHead := gitOut(repo, "rev-parse", "HEAD")
		if parentHead == "" {
			return intent, false, errors.New("parent has no exact HEAD for task landing")
		}
		if parentHead != intent.ParentBefore {
			intent.ParentBefore = parentHead
			if err := writeLandIntent(repo, intent); err != nil {
				return intent, false, err
			}
		}
		if gitOut(ws, "rev-parse", "HEAD") != intent.Candidate.Head {
			if err := restoreCandidateWorkspace(repo, ws, name, intent.Candidate); err != nil {
				return intent, false, err
			}
		}
		if err := c.rebaseForkOntoParent(repo, ws, name); err != nil {
			return intent, false, err
		}
		intent.RebasedHead = gitOut(ws, "rev-parse", "HEAD")
		intent.RebasedTree = gitOut(ws, "rev-parse", "HEAD^{tree}")
		intent.Phase = landRebased
		if !validObjectID(intent.RebasedHead) || !validObjectID(intent.RebasedTree) {
			return intent, false, errors.New("rebased fork has no exact HEAD/tree")
		}
		if err := writeLandIntent(repo, intent); err != nil {
			return intent, false, err
		}
	}
	if intent.Phase == landRebased {
		if gitOut(ws, "rev-parse", "HEAD") != intent.RebasedHead || gitOut(ws, "rev-parse", "HEAD^{tree}") != intent.RebasedTree {
			return intent, false, errors.New("rebased fork changed before its merge gate")
		}
		if img != "" && !c.gatePasses(repo, ws, img) {
			intent.Phase = landRestoring
			if err := writeLandIntent(repo, intent); err != nil {
				return intent, false, err
			}
			return c.finishRejectedTaskLand(repo, ws, name, intent)
		}
		intent.Phase = landApproved
		if err := writeLandIntent(repo, intent); err != nil {
			return intent, false, err
		}
	}
	if intent.Phase == landApproved {
		if err := gitFetchInto(repo, ws, name); err != nil {
			return intent, false, err
		}
		release, err := tasks.LockRefAuthority(c.cfg, repo)
		if err != nil {
			return intent, false, err
		}
		parentContains := gitRun(repo, "merge-base", "--is-ancestor", intent.RebasedHead, "HEAD") == nil
		if !parentContains {
			parentHead := gitOut(repo, "rev-parse", "HEAD")
			if parentHead != intent.ParentBefore {
				release()
				reset, resetErr := resetLandAfterParentAdvance(repo, ws, name, intent, parentHead)
				if resetErr != nil {
					return intent, false, errors.Join(fmt.Errorf("%s: parent advanced after candidate approval", name), resetErr)
				}
				return reset, false, fmt.Errorf("%s: parent advanced after candidate approval — candidate restored; retry the merge", name)
			}
			if gitOut(repo, "rev-parse", "review/"+name) != intent.RebasedHead {
				release()
				return intent, false, errors.New("fetched fork ref does not match approved rebased HEAD")
			}
			if err := gitRun(repo, "merge", "--ff-only", "review/"+name); err != nil {
				release()
				return intent, false, fmt.Errorf("%s: parent fast-forward failed; nothing landed", name)
			}
			if c.afterLandFastForward != nil {
				if err := c.afterLandFastForward(); err != nil {
					release()
					return intent, true, err
				}
			}
		}
		intent.Phase = landLanded
		writeErr := writeLandIntent(repo, intent)
		release()
		if writeErr != nil {
			return intent, true, writeErr
		}
	}
	if intent.Phase == landLanded {
		if gitRun(repo, "merge-base", "--is-ancestor", intent.RebasedHead, "HEAD") != nil {
			return intent, false, errors.New("land journal says landed but parent does not contain its exact commit")
		}
		for _, assignment := range intent.Candidate.Assignments {
			if err := tasks.FinalizeForkCandidateTask(repo, intent.Candidate, assignment); err != nil {
				return intent, true, fmt.Errorf("finalize canonical task %s after land: %w", assignment.Index.Task.Ref.ID, err)
			}
		}
		if err := tasks.RemoveForkCandidateIfMatchesLocked(repo, intent.Candidate); err != nil {
			return intent, true, err
		}
		if err := removeLandIntent(repo, intent); err != nil {
			return intent, true, err
		}
		return intent, true, nil
	}
	return intent, false, errors.New("unsupported fork land recovery state")
}
