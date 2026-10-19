package networkstate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
)

// Builds up to v11.3.0 filed a project's approval, withdrawal and private anchor under an id keyed
// with owner.key. This build names them by a plain hash (projectKey) and reads the keyed names for
// one upgrade: a withdrawal moves to the new name the first time the project is looked up, and an
// approval grants nothing but keeps the project's launches refused until a human approves the
// project again or forgets it, which retires it. A later build retires these readers as the
// waypoint table in .agent/kb/rules/one-upgrade-for-old-state.md says.

// legacyProjectKey is the keyed id; "" without a key, which no store holding such a record lacks.
func (s *Store) legacyProjectKey(canonical string) string {
	if len(s.key) != 32 {
		return ""
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("project-v1\x00" + canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// legacyApprovalPending refuses a project an older build approved and nobody approved since. The
// box can delete the checkout's marker and the project's settings, never this record, so the
// project's remembered posture cannot fall through to the open default.
func (s *Store) legacyApprovalPending(canonical string) *PendingApproval {
	legacy := s.legacyProjectKey(canonical)
	if legacy == "" {
		return nil
	}
	_, err := s.root.Lstat(approvalRecord(legacy))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return &PendingApproval{Reason: "this project's network approval cannot be verified — review it with 'coop approve'",
			Cause: "Coop could not read this project's network approval."}
	}
	return &PendingApproval{Reason: "coop renamed its network records — review this project's network access again with 'coop approve'",
		Cause: "This project's network access was approved before Coop renamed its network records."}
}

// legacyApproval is the approval an older build filed for this project and the id it is under. It
// is read only to show a review what the project had, or for `coop net forget` to take it back.
func (s *Store) legacyApproval(canonical string) (*Approval, string, error) {
	legacy := s.legacyProjectKey(canonical)
	if legacy == "" {
		return nil, "", nil
	}
	approval, err := s.approval(legacy)
	if approval == nil || err != nil {
		return nil, "", err
	}
	return approval, legacy, nil
}

// PriorApproval is the approval a review of this project starts from: the current one, or for one
// upgrade the one an older build filed. It only decides what a review proposes and shows; Approve
// publishes the reviewed After alone.
func (s *Store) PriorApproval(project string) (*Approval, error) {
	id, canonical, _, err := s.projectIdentity(project)
	if err != nil {
		return nil, err
	}
	current, err := s.approval(id)
	if current != nil || err != nil {
		return current, err
	}
	return s.shownLegacyApproval(canonical), nil
}

// shownLegacyApproval is the older build's approval a review shows when the project has no current
// one; one that cannot be read shows nothing, and approving retires it all the same.
func (s *Store) shownLegacyApproval(canonical string) *Approval {
	old, _, err := s.legacyApproval(canonical)
	if err != nil {
		return nil
	}
	return old
}

// retireLegacyApproval removes the approval an older build filed for this project once a fresh one
// is published. The checkout's marker is the fresh approval's by then, so of the old pair only the
// private anchor can be left: it goes first, while the record still names it. A record that cannot
// be read goes too, since the fresh approval supersedes it.
func (s *Store) retireLegacyApproval(canonical string) error {
	legacy := s.legacyProjectKey(canonical)
	if legacy == "" {
		return nil
	}
	if old, err := s.approval(legacy); err == nil && old != nil && projectAnchorRE.MatchString(old.ProjectAnchor) {
		if err := s.removeRecord(projectAnchorName(legacy, old.ProjectAnchor)); err != nil {
			return err
		}
	}
	return s.removeRecord(approvalRecord(legacy))
}

// moveWithdrawal files an older build's withdrawal under the project's id. It reads both names
// again under the approval's lock, which Approve holds while it lifts a withdrawal, so a launch
// never moves back one a fresh approval has just lifted.
func (s *Store) moveWithdrawal(legacy, id string) (bool, error) {
	found := false
	err := s.lockRecord(context.Background(), "approval", id, func() error {
		var err error
		if found, err = s.readWithdrawal(id, withdrawalVersion); found || err != nil {
			return err
		}
		if found, err = s.readWithdrawal(legacy, withdrawalLegacyVersion); !found || err != nil {
			return err
		}
		if err := s.recordWithdrawal(id); err != nil {
			return err
		}
		return s.removeWithdrawal(legacy)
	})
	return found, err
}
