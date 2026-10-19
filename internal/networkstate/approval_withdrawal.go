package networkstate

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/AndrewDryga/coop/internal/egress"
)

// Withdrawing an approval has to fail CLOSED, and deleting the record is not
// enough to do that: with the approval gone and the project's YAML gone too,
// mode resolution falls through to coop's built-in default, which is open. So
// forgetting writes a marker FIRST and removes the grant second — after a
// withdrawal this project's ordinary launches stop until a human approves
// again, and the only thing that clears the marker is that fresh approval.
//
// The marker is deliberately tiny: it is not a second kind of authority, it is
// the absence of authority made durable. It is filed under the same
// project id as the approval, so a copy of one project's marker cannot speak
// for another, and it is read by the same strict JSON path.

func withdrawalRecord(id string) string { return "withdrawn-" + id + ".json" }

// withdrawalVersion is a withdrawal filed under the project's id. Version 1 is one an older build
// filed under the keyed id (legacyProjectKey); it is moved to version 2 the first time the project
// is looked up.
const (
	withdrawalVersion       = 2
	withdrawalLegacyVersion = 1
)

// Withdrawal is the persisted barrier. Version and ProjectID are checked on
// read for exactly the reason an approval checks them: a record that does not
// name this project is not evidence about this project.
type Withdrawal struct {
	Version   int    `json:"version"`
	ProjectID string `json:"project_id"`
}

// withdrawn reports whether this project's approval was withdrawn on this host.
// An unreadable or malformed marker is an ERROR, never a quiet false: a barrier
// coop cannot read is not a barrier it may ignore. A withdrawal an older build
// filed under the project's keyed id moves to its id the first time it is looked
// up: a withdrawn project must never fall back to the open default because its
// records were renamed.
func (s *Store) withdrawn(id, canonical string) (bool, error) {
	found, err := s.readWithdrawal(id, withdrawalVersion)
	if found || err != nil {
		return found, err
	}
	legacy := s.legacyProjectKey(canonical)
	if legacy == "" {
		return false, nil
	}
	if found, err := s.readWithdrawal(legacy, withdrawalLegacyVersion); !found || err != nil {
		return found, err
	}
	return s.moveWithdrawal(legacy, id)
}

func (s *Store) readWithdrawal(id string, version int) (bool, error) {
	data, err := s.read(withdrawalRecord(id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record Withdrawal
	if err := json.Unmarshal(data, &record); err != nil {
		return false, err
	}
	if record.Version != version || record.ProjectID != id {
		return false, errors.New("network withdrawal identity mismatch")
	}
	return true, nil
}

// Withdrawn reports whether this project's approval was withdrawn on this host and not approved
// again since: a running editor session that is refused names the withdrawal, not a review.
func (s *Store) Withdrawn(project string) (bool, error) {
	id, canonical, _, err := s.projectIdentity(project)
	if err != nil {
		return false, err
	}
	return s.withdrawn(id, canonical)
}

// recordWithdrawal persists the barrier and fsyncs it. It runs BEFORE the
// approval is removed, so a crash between the two leaves the safe half done:
// a barrier with a stale grant still refuses, a grant with no barrier is what
// existed a moment ago.
func (s *Store) recordWithdrawal(id string) error {
	data, err := json.Marshal(Withdrawal{Version: withdrawalVersion, ProjectID: id})
	if err != nil {
		return err
	}
	return s.publish(withdrawalRecord(id), data, true)
}

// clearWithdrawal removes the barrier, under the project's id and the keyed one an
// older build used, which would otherwise move back on the next lookup. Only
// Approve calls it, inside the same lock that publishes the approval a human just
// confirmed.
func (s *Store) clearWithdrawal(id, canonical string) error {
	if legacy := s.legacyProjectKey(canonical); legacy != "" {
		if err := s.removeWithdrawal(legacy); err != nil {
			return err
		}
	}
	return s.removeWithdrawal(id)
}

func (s *Store) removeWithdrawal(id string) error { return s.removeRecord(withdrawalRecord(id)) }

// withdrawalBarrier is the pending review a withdrawal leaves behind. It
// applies only where access could be REGAINED: an offline run has nothing to
// widen, and a project that has been approved again has no barrier left.
// Everything else — including the built-in open default that made the
// withdrawal necessary — waits for a human.
func withdrawalBarrier(withdrawn bool, mode egress.Mode) *PendingApproval {
	if !withdrawn || mode == egress.None {
		return nil
	}
	return &PendingApproval{
		Reason: "this project's network approval was withdrawn on this machine",
		Cause:  "This project's network approval was withdrawn on this machine.",
	}
}
