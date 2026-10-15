package networkstate

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// The approval generation tells a long-running supervisor — a running `coop acp` editor session —
// that some project's network approval changed on this host, so it can admit again and see
// whether its own did. It is ONE host-wide counter on purpose: it names no project, so it lists
// nothing the store refuses to list, and it grants nothing. A reader that sees it move only
// knows to ask admission again; admission still decides everything.
//
// Writers advance it after the change it reports is durable, so a reader that sees the new value
// and admits again reads the new approval, never the one it replaced. Readers compare for any
// difference, never for an increase.

const approvalGenerationRecord = "approval-generation.json"

// ErrSessionsNotTold is an approval change that was saved but could not be announced: new runs
// use it, and a running editor session uses it once it reconnects.
var ErrSessionsNotTold = errors.New("running editor sessions were not told about this change")

type approvalGeneration struct {
	Version    int    `json:"version"`
	Generation uint64 `json:"generation"`
}

// ApprovalGeneration reads the counter: zero until an approval changes on this host. A record
// coop cannot read is an error, never zero, so a reader cannot mistake it for "nothing changed".
func (s *Store) ApprovalGeneration() (uint64, error) {
	data, err := s.read(approvalGenerationRecord, 256)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var record approvalGeneration
	if err := strictJSON(data, &record); err != nil {
		return 0, err
	}
	if record.Version != 1 || record.Generation == 0 {
		return 0, errors.New("network approval generation is malformed")
	}
	return record.Generation, nil
}

// announceApprovalChange advances the counter one step. Approvals of different projects hold
// different record locks, so the step takes a lock of its own: a lost update would let a
// supervisor that read the first of two steps miss the second. A failure leaves the change saved
// for new runs and returns ErrSessionsNotTold.
func (s *Store) announceApprovalChange(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ExecutionLockTimeout)
	defer cancel()
	err := s.lockFile(ctx, "approval-generation-lock", func() error {
		current, err := s.ApprovalGeneration()
		next := current + 1
		if err != nil || current == 0 {
			// A missing or unreadable counter starts again at a random point, never at 1: a value
			// a supervisor already saw would hide this change, and an unreadable one is repaired.
			if next, err = randomApprovalGeneration(); err != nil {
				return err
			}
		}
		data, err := json.Marshal(approvalGeneration{Version: 1, Generation: next})
		if err != nil {
			return err
		}
		return s.publish(approvalGenerationRecord, data, true)
	})
	if err != nil {
		return fmt.Errorf("%w (%v)", ErrSessionsNotTold, err)
	}
	return nil
}

// randomApprovalGeneration is a fresh starting point: never zero, which means "nothing changed
// yet", and with room to count up from it.
func randomApprovalGeneration() (uint64, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(raw[:])>>1 | 1, nil
}
