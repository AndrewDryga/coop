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
// the absence of authority made durable. It is filed under the same keyed
// project id as the approval, so a copy of one project's marker cannot speak
// for another, and it is read by the same strict JSON path.

func withdrawalRecord(id string) string { return "withdrawn-" + id + ".json" }

// Withdrawal is the persisted barrier. Version and ProjectID are checked on
// read for exactly the reason an approval checks them: a record that does not
// name this project is not evidence about this project.
type Withdrawal struct {
	Version   int    `json:"version"`
	ProjectID string `json:"project_id"`
}

// withdrawn reports whether this project's approval was withdrawn on this host.
// An unreadable or malformed marker is an ERROR, never a quiet false: a barrier
// coop cannot read is not a barrier it may ignore.
func (s *Store) withdrawn(id string) (bool, error) {
	data, err := s.read(withdrawalRecord(id), maxPrivateRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record Withdrawal
	if err := strictJSON(data, &record); err != nil {
		return false, err
	}
	if record.Version != 1 || record.ProjectID != id {
		return false, errors.New("network withdrawal identity mismatch")
	}
	return true, nil
}

// recordWithdrawal persists the barrier and fsyncs it. It runs BEFORE the
// approval is removed, so a crash between the two leaves the safe half done:
// a barrier with a stale grant still refuses, a grant with no barrier is what
// existed a moment ago.
func (s *Store) recordWithdrawal(id string) error {
	data, err := json.Marshal(Withdrawal{Version: 1, ProjectID: id})
	if err != nil {
		return err
	}
	return s.publish(withdrawalRecord(id), data, true)
}

// clearWithdrawal removes the barrier. Only Approve calls it, inside the same
// lock that publishes the approval a human just confirmed.
func (s *Store) clearWithdrawal(id string) error {
	err := s.root.Remove(withdrawalRecord(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := s.root.Lstat(withdrawalRecord(id)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("this network withdrawal could not be proven removed")
	}
	return s.confirmPublication()
}

// withdrawalBarrier is the pending review a withdrawal leaves behind. It
// applies only where access could be REGAINED: a host-owned named policy keeps
// its own authority, an offline run has nothing to widen, and a project that
// has been approved again has no barrier left. Everything else — including the
// built-in open default that made the withdrawal necessary — waits for a human.
func (a Admission) withdrawalBarrier(approval *Approval, withdrawn bool, mode egress.Mode) *PendingApproval {
	if !withdrawn || approval != nil || a.PolicyMode != nil || mode == egress.None {
		return nil
	}
	return &PendingApproval{
		Reason: "this project's network approval was withdrawn on this machine",
		Cause:  "This project's network approval was withdrawn on this machine.",
	}
}
