package workerconnector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Transport ACK and local retirement are different facts. An ACKed receipt
// whose origin cannot yet be preserved stays byte-for-byte on disk, but must
// yield the wire to fresh results and activity. Restart requires a fresh ACK.
type acknowledgedResult struct {
	commandDigest string
	resultDigest  [32]byte
	nextAttempt   time.Time
	delay         time.Duration
}

type acknowledgedResults map[string]acknowledgedResult

func acknowledgedIdentity(entry journalEntry) (acknowledgedResult, bool) {
	if entry.State != "completed" || entry.Command == nil || entry.Result == nil ||
		entry.Result.Validate() != nil || entry.Result.CommandID != entry.CommandID ||
		entry.Result.OperationKey != entry.Command.IdempotencyKey {
		return acknowledgedResult{}, false
	}
	encoded, err := encodeWireJSON(entry.Result)
	if err != nil {
		return acknowledgedResult{}, false
	}
	return acknowledgedResult{commandDigest: entry.CommandDigest, resultDigest: sha256.Sum256(encoded)}, true
}

func (a acknowledgedResults) matches(entry journalEntry) bool {
	identity, valid := acknowledgedIdentity(entry)
	saved, exists := a[entry.CommandID]
	return valid && exists && identity.commandDigest == saved.commandDigest && identity.resultDigest == saved.resultDigest
}

func (a acknowledgedResults) deferResult(entry journalEntry, now time.Time) {
	identity, valid := acknowledgedIdentity(entry)
	if !valid {
		return
	}
	delay := 5 * time.Second
	if a.matches(entry) {
		delay = min(max(a[entry.CommandID].delay*2, delay), 2*time.Minute)
	}
	identity.delay, identity.nextAttempt = delay, now.Add(delay)
	a[entry.CommandID] = identity
}

func (a acknowledgedResults) unsent(entries []journalEntry) []journalEntry {
	present := make(map[string]bool, len(entries))
	unsent := make([]journalEntry, 0, len(entries))
	for _, entry := range entries {
		if a.matches(entry) {
			present[entry.CommandID] = true
		} else {
			unsent = append(unsent, entry)
		}
	}
	for id := range a {
		if !present[id] {
			delete(a, id)
		}
	}
	return unsent
}

func (a acknowledgedResults) nextDue(entries []journalEntry, now time.Time) *journalEntry {
	var due *journalEntry
	for _, entry := range entries {
		if !a.matches(entry) || now.Before(a[entry.CommandID].nextAttempt) {
			continue
		}
		if due == nil || a[entry.CommandID].nextAttempt.Before(a[due.CommandID].nextAttempt) {
			copy := entry
			due = &copy
		}
	}
	return due
}

func (e *Executor) acknowledgeResult(ctx context.Context, entry journalEntry) error {
	err := e.journal.acknowledgeResult(entry)
	if !errors.Is(err, errCreateOriginConflict) {
		return err
	}
	if repair := e.retireFailedCreate(ctx, entry); repair != nil {
		return errors.Join(err, repair)
	}
	return nil
}

func (e *Executor) retireFailedCreate(ctx context.Context, entry journalEntry) error {
	origin, err := createOriginFromReceipt(entry)
	if err != nil {
		return err
	}
	if origin.OperationID == "" || origin.CoopSessionID != "" {
		return errCreateOriginConflict
	}
	var before createOrigin
	err = e.journal.withActivityLock(func() error {
		var readErr error
		before, readErr = e.journal.readCreateOrigin(e.journal.createOriginPath(origin.SessionRef))
		if readErr != nil {
			return readErr
		}
		if !conflictingFailedOrigin(before, origin) {
			return errCreateOriginConflict
		}
		return nil
	})
	if err != nil {
		return err
	}
	query := url.Values{"key": {origin.OperationKey}}
	resource, err := e.api.Do(ctx, Request{Method: "GET", Path: "/v1/operations?" + query.Encode()})
	if err != nil {
		return err
	}
	var operation createOperation
	if len(resource) > maxPrivateResponseBytes || json.Unmarshal(resource, &operation) != nil ||
		operation.ID != origin.OperationID || operation.Method != "CreateRemoteSession" || operation.State != "failed" ||
		operation.ResourceType != "" || operation.ResourceID != "" {
		return fmt.Errorf("worker create %s has no exact failed operation proof", entry.CommandID)
	}
	return e.journal.withActivityLock(func() error {
		current, err := e.journal.readCreateOrigin(e.journal.createOriginPath(origin.SessionRef))
		if err != nil {
			return err
		}
		if current != before {
			return errors.New("worker create origin changed during acknowledged receipt repair")
		}
		// The accepted operation failed without a resource. Keep the original
		// generation watermark and every stream/cursor; only retire this ACKed receipt.
		return e.journal.removeAcknowledgedResult(entry)
	})
}

func conflictingFailedOrigin(current, incoming createOrigin) bool {
	return current.WorkerID == incoming.WorkerID && current.SessionRef == incoming.SessionRef &&
		current.PlacementGeneration == incoming.PlacementGeneration && current.State == "failed" &&
		current.CoopSessionID == "" && !sameCreateOrigin(current, incoming)
}

// Only the exact results in this validated poll receive new transport authority.
func (c *Connector) acceptAcknowledgements(poll workerproto.Poll, entries []journalEntry, ids []string) error {
	byID := make(map[string]journalEntry, len(entries))
	for _, entry := range entries {
		byID[entry.CommandID] = entry
	}
	sent := make(map[string]workerproto.CommandResult, len(poll.CommandResults))
	for _, result := range poll.CommandResults {
		sent[result.CommandID] = result
	}
	var failures error
	for _, id := range ids {
		entry, exists := byID[id]
		result, offered := sent[id]
		if !exists || !offered || entry.Result == nil || !sameResult(result, *entry.Result) || c.acknowledged.matches(entry) {
			continue
		}
		err := c.executor.journal.acknowledgeResult(entry)
		if err == nil {
			continue
		}
		if errors.Is(err, errCreateOriginConflict) {
			if identity, valid := acknowledgedIdentity(entry); valid {
				identity.nextAttempt = c.now()
				c.acknowledged[id] = identity
			}
		} else {
			c.acknowledged.deferResult(entry, c.now())
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// One bounded original-key read per poll, after fresh commands and event ACKs.
func (c *Connector) repairAcknowledgement(ctx context.Context, entries []journalEntry) error {
	entry := c.acknowledged.nextDue(entries, c.now())
	if entry == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := c.executor.acknowledgeResult(ctx, *entry); err != nil {
		c.acknowledged.deferResult(*entry, c.now())
		return err
	}
	delete(c.acknowledged, entry.CommandID)
	return nil
}
