package forkctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

type isolatedLandIntent struct {
	Version            int                  `json:"version"`
	Fork               forkspace.Identity   `json:"fork"`
	SourceHead         string               `json:"source_head"`
	SourceTree         string               `json:"source_tree"`
	Upstream           string               `json:"upstream"`
	Parent             isolatedParent       `json:"parent"`
	PublicationHead    string               `json:"publication_head"`
	PublicationTree    string               `json:"publication_tree"`
	PublicationContent string               `json:"publication_content,omitempty"`
	Custody            string               `json:"custody"`
	Candidate          *tasks.ForkCandidate `json:"candidate,omitempty"`
	Phase              string               `json:"phase"`
	CreatedAt          time.Time            `json:"created_at"`
}

type isolatedSourceRecord struct {
	Version         int                `json:"version"`
	Fork            forkspace.Identity `json:"fork"`
	Head            string             `json:"head"`
	Tree            string             `json:"tree"`
	PublicationHead string             `json:"publication_head,omitempty"`
	PublicationTree string             `json:"publication_tree,omitempty"`
}

func isolatedSourcePath(repo string, identity forkspace.Identity) string {
	return filepath.Join(forkspace.StateDir(repo), ".isolated-source-"+string(identity.Generation)+".json")
}

func isolatedSourceBoundary(repo string, identity forkspace.Identity, parentHead string) (string, error) {
	var record isolatedSourceRecord
	ok, err := readIsolatedRecord(isolatedSourcePath(repo, identity), &record)
	if err != nil {
		return "", err
	}
	if !ok {
		base, err := forkspace.IsolatedCreationBase(repo, identity)
		if err != nil {
			return "", err
		}
		if _, err := observed(repo, "merge-base", "--is-ancestor", base, parentHead); err != nil {
			return "", fmt.Errorf("isolated creation base is no longer in the captured parent; preserve the fork and reconcile explicitly: %w", err)
		}
		return base, nil
	}
	if err := validateIsolatedSource(record, identity); err != nil {
		return "", errors.New("invalid isolated published source boundary")
	}
	publication := record.Head
	if record.Version == 2 {
		publication = record.PublicationHead
		tree, err := observed(repo, "rev-parse", "--verify", publication+"^{tree}")
		if err != nil || tree != record.PublicationTree {
			return "", errors.Join(errors.New("isolated source cutoff has no matching publication tree; preserve the fork and reconcile explicitly"), err)
		}
	}
	if _, err := observed(repo, "merge-base", "--is-ancestor", publication, parentHead); err != nil {
		return "", fmt.Errorf("isolated source cutoff is no longer published in the captured parent; preserve the fork and reconcile explicitly: %w", err)
	}
	tree, err := observed(forkspace.Workspace(repo, identity.Name), "rev-parse", "--verify", record.Head+"^{tree}")
	if err != nil || tree != record.Tree {
		return "", errors.Join(errors.New("isolated source cutoff tree changed"), err)
	}
	return record.Head, nil
}

func validateIsolatedSource(record isolatedSourceRecord, identity forkspace.Identity) error {
	if (record.Version != 1 && record.Version != 2) || record.Fork != identity ||
		!validObjectID(record.Head) || !validObjectID(record.Tree) ||
		record.Version == 1 && (record.PublicationHead != "" || record.PublicationTree != "") ||
		record.Version == 2 && (!validObjectID(record.PublicationHead) || !validObjectID(record.PublicationTree)) {
		return errors.New("invalid isolated source publication receipt")
	}
	return nil
}

// Only a completed exact-generation publication retained in the parent's current
// ancestry grants removal authority. A source boundary alone is not that proof.
func isolatedSourcePublished(repo, workspace string, identity forkspace.Identity) bool {
	if err := forkspace.ValidateGenerationWorkspace(repo, identity); err != nil {
		return false
	}
	if _, pending, err := readIsolatedIntent(repo, identity); err != nil || pending {
		return false
	}
	head, err := observed(workspace, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return false
	}
	if _, err := observed(repo, "merge-base", "--is-ancestor", head, "HEAD"); err == nil {
		return true // untouched creation or direct publication needs no rewrite receipt
	}
	var record isolatedSourceRecord
	if ok, err := readIsolatedRecord(isolatedSourcePath(repo, identity), &record); err != nil || !ok ||
		validateIsolatedSource(record, identity) != nil || record.Version != 2 {
		return false
	}
	if head != record.Head {
		return false
	}
	tree, err := observed(workspace, "rev-parse", "--verify", head+"^{tree}")
	if err != nil || tree != record.Tree {
		return false
	}
	tree, err = observed(repo, "rev-parse", "--verify", record.PublicationHead+"^{tree}")
	if err != nil || tree != record.PublicationTree {
		return false
	}
	_, err = observed(repo, "merge-base", "--is-ancestor", record.PublicationHead, "HEAD")
	return err == nil
}

func readIsolatedRecord(path string, target any) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() < 0 || info.Size() > landIntentLimit {
		return false, errors.New("isolated record is not a bounded independent regular file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(info, after) {
		return false, errors.Join(errors.New("isolated record changed while opening"), err)
	}
	data, err := io.ReadAll(io.LimitReader(file, landIntentLimit+1))
	if err != nil || len(data) > landIntentLimit {
		return false, errors.Join(errors.New("isolated record exceeds its bound"), err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return false, errors.Join(errors.New("isolated record contains trailing data"), err)
	}
	return true, nil
}

// published is true even when the containing-directory sync fails. Callers must
// then preserve everything the visible record references for conservative replay.
func writeIsolatedRecord(path string, value any) (published bool, err error) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > landIntentLimit {
		return false, errors.Join(errors.New("isolated record exceeds its bound"), err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".isolated-record-")
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(append(data, '\n')); err != nil {
		return false, errors.Join(err, file.Close())
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return false, err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return false, err
	}
	return true, syncIsolatedPath(filepath.Dir(path))
}

func syncIsolatedPath(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func validateIsolatedIntent(repo string, identity forkspace.Identity, intent isolatedLandIntent) error {
	if intent.Version != 2 || intent.Fork != identity || intent.Parent.Repo != repo || intent.CreatedAt.IsZero() ||
		!validObjectID(intent.SourceHead) || !validObjectID(intent.SourceTree) || !validObjectID(intent.Upstream) ||
		!validObjectID(intent.Parent.Head) || !validObjectID(intent.Parent.Tree) || len(intent.Parent.Index) != 64 ||
		len(intent.Parent.Content) != 64 || !validObjectID(intent.Parent.Content) ||
		intent.PublicationContent != "" && (len(intent.PublicationContent) != 64 || !validObjectID(intent.PublicationContent)) ||
		intent.Phase == "landed" && intent.PublicationContent == "" ||
		!validObjectID(intent.PublicationHead) || !validObjectID(intent.PublicationTree) ||
		!strings.HasPrefix(intent.Parent.Target, "refs/heads/") || len(intent.Parent.Metadata) == 0 ||
		filepath.Base(intent.Custody) != intent.Custody || !strings.HasPrefix(intent.Custody, ".isolated-candidate-") ||
		(intent.Phase != "started" && intent.Phase != "landed") {
		return errors.New("invalid isolated v2 land intent")
	}
	if intent.Candidate != nil {
		if err := tasks.ValidateForkCandidateRecord(*intent.Candidate); err != nil {
			return err
		}
		if intent.Candidate.Fork != identity || intent.Candidate.Head != intent.SourceHead || intent.Candidate.Tree != intent.SourceTree {
			return errors.New("isolated task authority does not bind the original source")
		}
	}
	return nil
}

func readIsolatedIntent(repo string, identity forkspace.Identity) (isolatedLandIntent, bool, error) {
	var intent isolatedLandIntent
	ok, err := readIsolatedRecord(landIntentPath(repo, identity), &intent)
	if err != nil || !ok {
		return intent, ok, err
	}
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return intent, true, err
	}
	return intent, true, validateIsolatedIntent(canonical, identity, intent)
}

func intentCandidate(repo string, intent isolatedLandIntent) isolatedCandidate {
	return isolatedCandidate{dir: filepath.Join(forkspace.StateDir(repo), intent.Custody), identity: intent.Fork,
		parent: intent.Parent, sourceHead: intent.SourceHead, sourceTree: intent.SourceTree,
		upstream: intent.Upstream, head: intent.PublicationHead, tree: intent.PublicationTree}
}

func syncIsolatedCustody(candidate isolatedCandidate) error {
	if err := forkspace.ValidateIndependentGit(candidate.dir); err != nil {
		return err
	}
	// Only immutable Git/object/LFS custody is needed for replay. Worktree symlinks
	// are committed data, never followed while syncing metadata.
	err := filepath.WalkDir(filepath.Join(candidate.dir, ".git"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return syncIsolatedPath(path)
	})
	return errors.Join(err, syncIsolatedPath(candidate.dir), syncIsolatedPath(filepath.Dir(candidate.dir)))
}

func (c *Control) advanceIsolatedLand(repo string, intent isolatedLandIntent) (bool, error) {
	candidate := intentCandidate(repo, intent)
	if err := candidate.validateSource(repo); err != nil {
		return false, err
	}
	if err := forkspace.ValidateIndependentGit(candidate.dir); err != nil {
		return false, fmt.Errorf("preserved isolated custody is invalid: %w", err)
	}
	head, err := observed(candidate.dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || head != intent.PublicationHead {
		return false, errors.Join(errors.New("retained isolated candidate changed"), err)
	}
	tree, err := observed(candidate.dir, "rev-parse", "--verify", head+"^{tree}")
	if err != nil || tree != intent.PublicationTree {
		return false, errors.Join(errors.New("retained isolated candidate tree changed"), err)
	}
	if err := forkspace.VerifyLFS(context.Background(), candidate.dir, intent.PublicationHead); err != nil {
		return false, fmt.Errorf("preserved isolated LFS custody is invalid: %w", err)
	}
	unlock, err := tasks.LockRefAuthority(c.cfg, repo)
	if err != nil {
		return false, err
	}
	defer unlock()
	parent, err := captureIsolatedParent(repo)
	if err != nil {
		return false, fmt.Errorf("isolated publication interrupted or parent changed; custody/journal retained, reconcile without resetting: %w", err)
	}
	landed := parent.Head == intent.PublicationHead && parent.Tree == intent.PublicationTree &&
		intent.PublicationContent != "" && parent.Content == intent.PublicationContent &&
		parent.Repo == intent.Parent.Repo && parent.Target == intent.Parent.Target &&
		strings.Join(parent.Metadata, "\x00") == strings.Join(intent.Parent.Metadata, "\x00")
	if !landed {
		if intent.Phase == "landed" || !sameIsolatedParent(parent, intent.Parent) {
			return false, errors.New("isolated parent is neither unchanged nor coherently landed on the recorded target; preserve journal/custody and reconcile")
		}
		if err := checkIsolatedIntroductions(repo, candidate.dir, intent.Parent.Head, intent.PublicationHead); err != nil {
			return false, err
		}
		if intent.Candidate != nil {
			if err := tasks.ValidateForkCandidateLocked(repo, *intent.Candidate, intent.SourceHead, intent.SourceTree); err != nil {
				return false, err
			}
		}
		ctx := context.Background()
		if err := forkspace.ImportIsolatedObjects(ctx, candidate.dir, repo, intent.PublicationHead); err != nil {
			return false, fmt.Errorf("isolated publication started; journal/custody retained: %w", err)
		}
		if err := forkspace.PrepareIsolatedLFSCheckout(ctx, repo, intent.Parent.Head, intent.PublicationHead); err != nil {
			return false, fmt.Errorf("isolated LFS checkout preparation interrupted; preserve checkout and journal: %w", err)
		}
		if err := forkspace.CheckoutIsolatedTree(ctx, repo, intent.Parent.Head, intent.PublicationHead); err != nil {
			return false, fmt.Errorf("isolated publication interrupted; do not reset or remove locks; journal/custody retained: %w", err)
		}
		if err := forkspace.PublishIsolatedLFS(ctx, candidate.dir, repo, intent.PublicationHead); err != nil {
			return false, fmt.Errorf("isolated LFS publication interrupted; preserve checkout and journal: %w", err)
		}
		checked, err := captureIsolatedCheckoutTree(repo, false, 0, intent.PublicationHead)
		if err != nil || checked.Head != intent.Parent.Head || checked.Target != intent.Parent.Target ||
			checked.Repo != intent.Parent.Repo || strings.Join(checked.Metadata, "\x00") != strings.Join(intent.Parent.Metadata, "\x00") {
			return false, errors.Join(errors.New("isolated checkout changed before ref publication; custody/journal retained"), err)
		}
		// Bind the verified hydrated/native checkout before CAS. A crash after
		// ref publication must not mint this proof from whatever bytes exist later.
		intent.PublicationContent = checked.Content
		if _, err := writeIsolatedRecord(landIntentPath(repo, intent.Fork), intent); err != nil {
			return false, err
		}
		cas := forkspace.GitRefCommand(ctx, repo, "update-ref", "-m", "Coop isolated publication", intent.Parent.Target, intent.PublicationHead, intent.Parent.Head)
		cas.Env = forkspace.IndependentGitEnv()
		if err := cas.Run(); err != nil {
			return false, fmt.Errorf("isolated ref publication interrupted; preserve checkout and journal: %w", err)
		}
		if c.afterLandFastForward != nil {
			if err := c.afterLandFastForward(); err != nil {
				return true, err
			}
		}
		confirmed, err := captureIsolatedParent(repo)
		if err != nil || confirmed.Head != intent.PublicationHead || confirmed.Tree != intent.PublicationTree ||
			confirmed.Content != intent.PublicationContent ||
			confirmed.Target != intent.Parent.Target || confirmed.Repo != intent.Parent.Repo ||
			strings.Join(confirmed.Metadata, "\x00") != strings.Join(intent.Parent.Metadata, "\x00") {
			return true, errors.Join(errors.New("isolated publication cannot be coherently confirmed; preserve journal/custody and reconcile"), err)
		}
		landed = true
	}
	if err := forkspace.VerifyLFS(context.Background(), repo, intent.PublicationHead); err != nil {
		return landed, fmt.Errorf("isolated published payload is incomplete or changed; preserve journal/custody and reconcile: %w", err)
	}
	intent.Phase = "landed"
	if _, err := writeIsolatedRecord(landIntentPath(repo, intent.Fork), intent); err != nil {
		return landed, err
	}
	if intent.Candidate != nil {
		for _, assignment := range intent.Candidate.Assignments {
			if err := tasks.FinalizeForkCandidateTask(repo, *intent.Candidate, assignment); err != nil {
				return true, fmt.Errorf("isolated Git landed; task finalization remains journaled: %w", err)
			}
		}
		if err := tasks.MarkForkCandidateLandedLocked(repo, *intent.Candidate); err != nil {
			return true, err
		}
	}
	record := isolatedSourceRecord{Version: 2, Fork: intent.Fork, Head: intent.SourceHead, Tree: intent.SourceTree,
		PublicationHead: intent.PublicationHead, PublicationTree: intent.PublicationTree}
	if _, err := writeIsolatedRecord(isolatedSourcePath(repo, intent.Fork), record); err != nil {
		return true, err
	}
	if err := os.Remove(landIntentPath(repo, intent.Fork)); err != nil {
		return true, err
	}
	if err := syncIsolatedPath(forkspace.StateDir(repo)); err != nil {
		return true, err
	}
	candidate.cleanup()
	return true, nil
}

// The lifecycle lock is already held by mergeOneMode. Exact custody replaces
// ordinary parent fetch/live-workspace rebase for every isolated generation.
func (c *Control) mergeIsolatedLocked(repo, img, name string, identity forkspace.Identity, force, skipEmpty bool, confirm func(isolatedCandidate) bool) (outcome mergeOutcome, retErr error) {
	pin, info, err := forkspace.Pin(forkspace.Workspace(repo, name))
	if err != nil {
		return mergeOutcome{}, err
	}
	defer func() {
		if outcome.approval == nil {
			_ = pin.Close()
		}
	}()
	finish := func(landed bool, source, publication string, err error) (mergeOutcome, error) {
		result := mergeOutcome{landed: landed}
		if !landed || err != nil {
			return result, err
		}
		approval := &landedFork{pin: pin, info: info, identity: identity, hasGeneration: true, head: source, publishedHead: publication}
		if err := approval.validateLand(repo, name); err != nil {
			return result, err
		}
		result.approval = approval
		return result, nil
	}
	if intent, pending, err := readIsolatedIntent(repo, identity); err != nil {
		return mergeOutcome{}, err
	} else if pending {
		ui.Note("Reconciling journaled isolated publication for %s on its recorded target.", name)
		landed, err := c.advanceIsolatedLand(repo, intent)
		return finish(landed, intent.SourceHead, intent.PublicationHead, err)
	}
	if force {
		return mergeOutcome{}, errors.New("--force cannot bypass isolated publication policy")
	}
	candidate, err := prepareIsolatedCandidate(repo, name, identity, forkspace.WantsSigning())
	if err != nil {
		return mergeOutcome{}, err
	}
	retain := false
	defer func() {
		if !retain {
			candidate.cleanup()
		}
	}()
	if skipEmpty && candidate.sourceHead == candidate.upstream {
		summary, err := tasks.ReadForkTaskStateSummary(repo, identity)
		if err != nil {
			return mergeOutcome{}, err
		}
		if !summary.Active() {
			return mergeOutcome{skipped: true}, nil
		}
	}
	if confirm != nil && !confirm(candidate) {
		return mergeOutcome{skipped: true}, nil
	}
	taskCandidate, hasCandidate, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil {
		return mergeOutcome{}, err
	}
	indexes, problems := tasks.IndexedForkAssignments(repo, identity)
	if len(problems) != 0 {
		return mergeOutcome{}, errors.Join(problems...)
	}
	if len(indexes) > 0 && !hasCandidate {
		return mergeOutcome{}, errors.New("isolated task fork needs its exact final reviewed candidate before land")
	}
	var original *tasks.ForkCandidate
	if hasCandidate {
		if err := tasks.ValidateForkCandidateLocked(repo, taskCandidate, candidate.sourceHead, candidate.sourceTree); err != nil {
			return mergeOutcome{}, err
		}
		original = &taskCandidate
	}
	if err := c.gateIsolatedCandidate(repo, img, candidate, false); err != nil {
		return mergeOutcome{}, err
	}
	if err := syncIsolatedCustody(candidate); err != nil {
		return mergeOutcome{}, err
	}
	intent := isolatedLandIntent{Version: 2, Fork: identity, SourceHead: candidate.sourceHead,
		SourceTree: candidate.sourceTree, Upstream: candidate.upstream, Parent: candidate.parent,
		PublicationHead: candidate.head, PublicationTree: candidate.tree, Custody: filepath.Base(candidate.dir),
		Candidate: original, Phase: "started", CreatedAt: time.Now().UTC()}
	if candidate.head == candidate.parent.Head && candidate.tree == candidate.parent.Tree &&
		forkspace.VerifyLFS(context.Background(), repo, candidate.head) == nil {
		intent.PublicationContent = candidate.parent.Content // exact no-effect publication
	}
	if err := validateIsolatedIntent(candidate.parent.Repo, identity, intent); err != nil {
		return mergeOutcome{}, err
	}
	// Refuse stale approval before creating any parent publication effect.
	parent, err := captureIsolatedParent(repo)
	if err != nil || !sameIsolatedParent(parent, candidate.parent) {
		return mergeOutcome{}, errors.Join(errors.New("parent changed after isolated review; publication not started"), err)
	}
	if err := candidate.validateSource(repo); err != nil {
		return mergeOutcome{}, err
	}
	published, err := writeIsolatedRecord(landIntentPath(repo, identity), intent)
	retain = published
	if err != nil {
		return mergeOutcome{}, err
	}
	landed, err := c.advanceIsolatedLand(repo, intent)
	return finish(landed, candidate.sourceHead, candidate.head, err)
}
