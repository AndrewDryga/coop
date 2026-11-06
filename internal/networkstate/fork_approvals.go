package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SnapshotForkApprovals carries existing authority to a newly created checkout. The caller owns
// the unpublished destination under its lifecycle lock. Resuming a fork must never call this:
// absence is part of the snapshot, and later parent approvals belong only to the parent.
func (s *Store) SnapshotForkApprovals(ctx context.Context, parent, fork, forkImage string) error {
	if err := s.intactAuthority(); err != nil {
		return err
	}
	parentID, parent, parentInfo, err := s.projectIdentity(parent)
	if err != nil {
		return err
	}
	forkID, fork, forkInfo, err := s.projectIdentity(fork)
	if err != nil {
		return err
	}
	if os.SameFile(parentInfo, forkInfo) {
		return errors.New("approval snapshot requires a new fork directory")
	}
	if !safeRecordToken(forkImage, 256) || strings.Contains(forkImage, ":") {
		return errors.New("approval snapshot requires the fork's project image name")
	}
	var approval *Approval
	var withdrawn bool
	var review string
	var reviewedFiles map[string]string
	err = s.lockRecord(ctx, "approval", parentID, func() error {
		approval, err = s.approval(parentID)
		if err != nil {
			return err
		}
		if err := s.checkDirectory(approval, parent); err != nil {
			return err
		}
		withdrawn, err = s.withdrawn(parentID)
		if err != nil {
			return err
		}
		// Build reviews publish by atomic rename. Copy that record, without waiting on a build
		// that may take minutes; a later completed build is a later parent approval.
		review, reviewedFiles, err = s.projectBuildReview(parent)
		return err
	})
	if err != nil {
		return err
	}
	// Release the source lock before publishing: the two IDs can share one flock stripe.
	return s.lockRecord(ctx, "approval", forkID, func() error {
		return s.snapshotForkApprovals(fork, forkID, forkImage, approval, withdrawn, review, reviewedFiles)
	})
}

func (s *Store) snapshotForkApprovals(fork, id, image string, approval *Approval, withdrawn bool, review string, reviewedFiles map[string]string) (err error) {
	if _, err := os.Lstat(filepath.Join(fork, ProjectApprovalMarker)); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("new fork already has a network approval marker"), err)
	}
	prior, err := s.approval(id)
	if err != nil {
		return err
	}
	// A withdrawal at the destination is still a host refusal, even when the name is reused.
	if withdrawn {
		if err := s.recordWithdrawal(id); err != nil {
			return err
		}
		approval = nil
	}
	// Reusing a fork name must not reuse the previous checkout's path-keyed build permission.
	// Its network anchor is stale too: this newly cloned directory has no public half.
	if err := s.retireProjectAnchor(fork, prior); err != nil {
		return err
	}
	for _, name := range []string{approvalRecord(id), projectBuildReviewName(projectBuildReviewID(fork))} {
		if err := s.removeRecord(name); err != nil {
			return err
		}
	}
	if err := s.removeForkImageApprovals(image); err != nil {
		return err
	}
	var copied *Approval
	defer func() {
		if err != nil {
			err = errors.Join(err, s.removeRecord(approvalRecord(id)),
				s.removeRecord(projectBuildReviewName(projectBuildReviewID(fork))), s.retireProjectAnchor(fork, copied))
		}
	}()
	if approval != nil {
		anchor, err := s.ensureProjectAnchor(fork, id, true, nil)
		if err != nil {
			return err
		}
		copied = copyApproval(approval)
		copied.ProjectID, copied.ProjectAnchor = id, anchor
		data, err := json.Marshal(copied)
		if err != nil {
			return err
		}
		if err := s.publish(approvalRecord(id), data, false); err != nil {
			return fmt.Errorf("snapshot fork network approval: %w", err)
		}
	}
	if review != "" {
		return s.RecordProjectBuildReview(fork, review, reviewedFiles)
	}
	return nil
}

// RetireForkApprovals retires what a fork checkout Coop is about to delete holds here: its network
// approval with the private half of its anchor, its reviewed build files and its image grants. A
// withdrawal at the path stays: it refuses rather than grants, and a fork recreated under the same
// name keeps it. The caller owns the stopped checkout under its lifecycle lock.
func (s *Store) RetireForkApprovals(ctx context.Context, fork, forkImage string) error {
	if err := s.intactAuthority(); err != nil {
		return err
	}
	id, fork, _, err := s.projectIdentity(fork)
	if err != nil {
		return err
	}
	if !safeRecordToken(forkImage, 256) || strings.Contains(forkImage, ":") {
		return errors.New("approval retirement requires the fork's project image name")
	}
	return s.lockRecord(ctx, "approval", id, func() error {
		approval, err := s.approval(id)
		if err != nil {
			return err
		}
		// The anchor goes while its record still names it: removing the record first would leave
		// a private link nothing could prove or collect.
		if err := s.retireProjectAnchor(fork, approval); err != nil {
			return fmt.Errorf("retire fork network approval identity: %w", err)
		}
		for _, name := range []string{approvalRecord(id), projectBuildReviewName(projectBuildReviewID(fork))} {
			if err := s.removeRecord(name); err != nil {
				return err
			}
		}
		return s.removeForkImageApprovals(forkImage)
	})
}

// A previous fork at this path may have approved images of its own. They are usable without the
// reviewed-files record, so a new fork must retire those grants too. This removes no Docker image.
func (s *Store) removeForkImageApprovals(image string) error {
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "projectbuild-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := s.read(entry.Name())
		if err != nil {
			return err
		}
		var record projectBuildRecord
		if json.Unmarshal(data, &record) != nil || !strings.HasPrefix(record.Tag, image+":") {
			continue
		}
		id, err := s.projectBuildID(record.Tag)
		if err != nil || entry.Name() != projectBuildRecordName(id) {
			continue
		}
		if err := s.removeRecord(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

// The reviewed-files digest omits the project path. The image-output record does not: its tag is
// part of the inputs digest. A fork therefore rebuilds only these already-reviewed files through
// the ordinary automatic-build path, producing its own image proof.
func (s *Store) projectBuildReview(project string) (string, map[string]string, error) {
	id := projectBuildReviewID(project)
	data, err := s.read(projectBuildReviewName(id))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var record projectBuildReviewRecord
	if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.ID != id ||
		record.Project != project || !lowerHex(record.Review, 64) {
		return "", nil, errors.New("parent project build review is invalid")
	}
	return record.Review, record.Files, nil
}
