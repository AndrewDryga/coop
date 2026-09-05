package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Origins outlive both command receipts and discarded streams. A bound/failed origin is also
// the generation watermark: replay must never resurrect an older placement or discarded stream.
type createOrigin struct {
	Version             int    `json:"version"`
	WorkerID            string `json:"worker_id"`
	SessionRef          string `json:"session_ref"`
	PlacementGeneration int    `json:"placement_generation"`
	CommandID           string `json:"command_id"`
	CommandDigest       string `json:"command_digest"`
	OperationKey        string `json:"operation_key"`
	OperationID         string `json:"operation_id"`
	State               string `json:"state"`
	CoopSessionID       string `json:"coop_session_id,omitempty"`
}

type createOperation struct {
	ID           string `json:"id"`
	Method       string `json:"method"`
	State        string `json:"state"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

func (j *journal) preserveCreateOrigin(entry journalEntry) error {
	if entry.Command == nil || entry.Result == nil || entry.Command.Kind != "create_session" || entry.Result.State != "succeeded" {
		return nil
	}
	return j.withActivityLock(func() error { return j.preserveCreateOriginLocked(entry) })
}

func (j *journal) preserveCreateOriginLocked(entry journalEntry) error {
	command, result := entry.Command, entry.Result
	computed, err := commandDigest(*command)
	if err != nil || computed != entry.CommandDigest || command.Validate() != nil || result.Validate() != nil ||
		entry.CommandID != command.CommandID || result.CommandID != command.CommandID || result.OperationKey != command.IdempotencyKey {
		return errors.New("worker create receipt identity is malformed")
	}
	var resource struct {
		Operation createOperation `json:"operation"`
		Session   struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(result.Resource, &resource); err != nil {
		return errors.New("worker create receipt resource is malformed")
	}
	op := resource.Operation
	// Older synchronous receipts proved session.id directly. Async receipts must prove the
	// accepted create operation instead; a later get_session/reconcile payload is never authority.
	if resource.Session.ID != "" {
		if !reference(resource.Session.ID, 1024) || (op.Method != "" && op.Method != "CreateRemoteSession") ||
			(op.ID != "" && !reference(op.ID, 256)) || (op.State != "" && op.State != "succeeded") ||
			(op.ResourceType != "" && op.ResourceType != "session") ||
			(op.ResourceID != "" && op.ResourceID != resource.Session.ID) {
			return errors.New("worker synchronous create identity conflicts")
		}
	} else if !reference(op.ID, 256) || op.Method != "CreateRemoteSession" || !createOperationState(op.State) {
		return errors.New("worker create receipt has no accepted operation identity")
	}
	origin := createOrigin{
		Version: 1, WorkerID: command.WorkerID, SessionRef: command.SessionRef, PlacementGeneration: command.PlacementGeneration,
		CommandID: command.CommandID, CommandDigest: computed, OperationKey: command.IdempotencyKey,
		OperationID: op.ID, State: "pending", CoopSessionID: resource.Session.ID,
	}
	current, err := j.readCreateOrigin(j.createOriginPath(origin.SessionRef))
	if err == nil {
		// A prior rename can be visible after its directory sync failed. Retry durability
		// before treating any existing generation marker as authority to release a receipt.
		if err := j.syncActivityDir(j.origins); err != nil {
			return err
		}
		if current.PlacementGeneration > origin.PlacementGeneration {
			return nil
		}
		if current.PlacementGeneration == origin.PlacementGeneration {
			if !sameCreateOrigin(current, origin) || (origin.CoopSessionID != "" && current.CoopSessionID != "" && origin.CoopSessionID != current.CoopSessionID) {
				return errors.New("worker create origin identity conflicts")
			}
			if current.State != "pending" || current.CoopSessionID == "" {
				return nil
			}
			return j.bindCreateOriginLocked(current, current.CoopSessionID)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stream, err := j.readEventStream(j.eventStreamPath(origin.SessionRef))
	if err == nil {
		if stream.PlacementGeneration > origin.PlacementGeneration {
			return j.syncActivityDir(j.streams)
		}
		if stream.PlacementGeneration == origin.PlacementGeneration && origin.CoopSessionID != "" && stream.CoopSessionID != origin.CoopSessionID {
			return errors.New("worker create origin conflicts with existing stream")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := j.writeActivityRecord(j.createOriginPath(origin.SessionRef), origin); err != nil {
		return err
	}
	if origin.CoopSessionID != "" {
		return j.bindCreateOriginLocked(origin, origin.CoopSessionID)
	}
	return nil
}

func createOperationState(state string) bool {
	return state == "reserved" || state == "running" || state == "uncertain" || state == "succeeded" || state == "failed"
}

func sameCreateOrigin(left, right createOrigin) bool {
	return left.WorkerID == right.WorkerID && left.SessionRef == right.SessionRef &&
		left.PlacementGeneration == right.PlacementGeneration && left.CommandID == right.CommandID &&
		left.CommandDigest == right.CommandDigest && left.OperationKey == right.OperationKey && left.OperationID == right.OperationID
}

func (j *journal) createOriginPath(sessionRef string) string {
	return filepath.Join(j.origins, filepath.Base(j.eventStreamPath(sessionRef)))
}

func (j *journal) readCreateOrigin(path string) (createOrigin, error) {
	file, err := os.Open(path)
	if err != nil {
		return createOrigin{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16<<10+1))
	if err != nil {
		return createOrigin{}, err
	}
	var origin createOrigin
	if len(data) > 16<<10 || json.Unmarshal(data, &origin) != nil || origin.Version != 1 ||
		!reference(origin.WorkerID, 256) || !reference(origin.SessionRef, 256) || origin.PlacementGeneration <= 0 ||
		!reference(origin.CommandID, 256) || !digest(origin.CommandDigest) || !reference(origin.OperationKey, 512) ||
		(origin.State != "pending" && origin.State != "bound" && origin.State != "failed") ||
		(origin.OperationID == "" && origin.CoopSessionID == "") ||
		(origin.OperationID != "" && !reference(origin.OperationID, 256)) ||
		(origin.CoopSessionID != "" && !reference(origin.CoopSessionID, 1024)) ||
		(origin.State == "bound" && origin.CoopSessionID == "") || path != j.createOriginPath(origin.SessionRef) {
		return createOrigin{}, errors.New("worker create origin is malformed")
	}
	return origin, nil
}

func (j *journal) bindCreateOrigin(origin createOrigin, sessionID string) error {
	return j.withActivityLock(func() error { return j.bindCreateOriginLocked(origin, sessionID) })
}

func (j *journal) bindCreateOriginLocked(origin createOrigin, sessionID string) error {
	current, err := j.readCreateOrigin(j.createOriginPath(origin.SessionRef))
	if err != nil {
		return err
	}
	if !sameCreateOrigin(current, origin) {
		return errors.New("worker create origin changed before binding")
	}
	if current.State != "pending" {
		return j.syncActivityDir(j.origins)
	}
	if !reference(sessionID, 1024) || (current.CoopSessionID != "" && current.CoopSessionID != sessionID) {
		return errors.New("worker create session identity conflicts")
	}
	if err := j.bindEventStream(workerproto.Command{SessionRef: origin.SessionRef, PlacementGeneration: origin.PlacementGeneration}, sessionID); err != nil {
		return err
	}
	// A pending origin suppresses the stream until BOTH publications are durable. On retry,
	// bindEventStream adopts the exact existing stream without resetting its published cursors.
	current.State, current.CoopSessionID = "bound", sessionID
	return j.writeActivityRecord(j.createOriginPath(origin.SessionRef), current)
}

func (e *Executor) resolveCreateOrigin(ctx context.Context, origin createOrigin) error {
	if origin.CoopSessionID != "" {
		return e.journal.bindCreateOrigin(origin, origin.CoopSessionID)
	}
	query := url.Values{"key": {origin.OperationKey}}
	resource, err := e.api.Do(ctx, Request{Method: "GET", Path: "/v1/operations?" + query.Encode()})
	if err != nil {
		return err
	}
	var operation createOperation
	if len(resource) > maxPrivateResponseBytes || json.Unmarshal(resource, &operation) != nil ||
		operation.ID != origin.OperationID || operation.Method != "CreateRemoteSession" || !createOperationState(operation.State) {
		return errors.New("worker create operation identity conflicts")
	}
	switch operation.State {
	case "succeeded":
		if operation.ResourceType != "session" {
			return errors.New("worker create operation has no session resource")
		}
		return e.journal.bindCreateOrigin(origin, operation.ResourceID)
	case "failed":
		return e.journal.withActivityLock(func() error {
			current, err := e.journal.readCreateOrigin(e.journal.createOriginPath(origin.SessionRef))
			if err != nil || !sameCreateOrigin(current, origin) || current.State != "pending" {
				return errors.New("worker create origin changed before failure")
			}
			current.State = "failed"
			return e.journal.writeActivityRecord(e.journal.createOriginPath(origin.SessionRef), current)
		})
	}
	return nil
}

type activityTarget struct {
	sessionRef string
	origin     *createOrigin
	stream     *eventStream
}

func (e *Executor) activityTargets() ([]activityTarget, error) {
	streams, err := e.journal.eventStreams()
	if err != nil {
		return nil, err
	}
	targets := make(map[string]activityTarget, len(streams))
	for _, stream := range streams {
		targets[stream.SessionRef] = activityTarget{sessionRef: stream.SessionRef, stream: &stream}
	}
	files, err := os.ReadDir(e.journal.origins)
	if err != nil {
		return nil, err
	}
	var issues error
	for _, file := range files {
		if filepath.Ext(file.Name()) != ".json" {
			continue
		}
		origin, err := e.journal.readCreateOrigin(filepath.Join(e.journal.origins, file.Name()))
		if err != nil {
			issues = errors.Join(issues, fmt.Errorf("worker create origin %s: %w", file.Name(), err))
			// The filename, not untrusted decoded identity, determines which legacy
			// stream must be suppressed. Healthy siblings can still narrate their work.
			for ref := range targets {
				if filepath.Base(e.journal.createOriginPath(ref)) == file.Name() {
					delete(targets, ref)
				}
			}
			continue
		}
		target := targets[origin.SessionRef]
		target.sessionRef = origin.SessionRef
		switch {
		case origin.WorkerID != e.workerID:
			target.stream = nil
		case target.stream != nil && target.stream.PlacementGeneration > origin.PlacementGeneration:
			// A newer legacy stream is a generation floor, not fabricated operation proof.
		case origin.State == "pending":
			target.origin, target.stream = &origin, nil
		case origin.State == "bound" && target.stream != nil && target.stream.PlacementGeneration == origin.PlacementGeneration && target.stream.CoopSessionID == origin.CoopSessionID:
		default:
			target.stream = nil
		}
		targets[origin.SessionRef] = target
	}
	result := make([]activityTarget, 0, len(targets))
	for _, target := range targets {
		if target.stream != nil && target.stream.TerminalSequence > 0 && target.stream.AcknowledgedSequence >= target.stream.TerminalSequence {
			continue // an acknowledged legacy stream is only a generation watermark
		}
		if target.origin != nil || target.stream != nil {
			result = append(result, target)
		}
	}
	sort.Slice(result, func(i, k int) bool { return result[i].sessionRef < result[k].sessionRef })
	if cursor, err := os.ReadFile(e.journal.eventScanPath()); err == nil {
		index := sort.Search(len(result), func(index int) bool { return result[index].sessionRef > string(cursor) })
		result = append(result[index:], result[:index]...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read worker activity scan cursor: %w", err)
	}
	return result, issues
}

func (j *journal) eventStreamAuthorized(stream eventStream) bool {
	origin, err := j.readCreateOrigin(j.createOriginPath(stream.SessionRef))
	return errors.Is(err, os.ErrNotExist) || (err == nil && j.syncActivityDir(j.origins) == nil && (origin.PlacementGeneration < stream.PlacementGeneration ||
		(origin.State == "bound" && origin.PlacementGeneration == stream.PlacementGeneration && origin.CoopSessionID == stream.CoopSessionID)))
}

// Separate processes can open one journal. Serialize its short metadata transitions, not API
// reads: a stale lookup must recheck authority under the same lock that publishes generations.
func (j *journal) withActivityLock(action func() error) error {
	path := filepath.Join(j.origins, ".authority.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock worker activity authority: %w", err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	opened, openErr := file.Stat()
	current, currentErr := os.Lstat(path)
	if openErr != nil || currentErr != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, current) {
		return errors.New("worker activity authority lock changed")
	}
	return action()
}

func (j *journal) syncActivityDir(directory string) error {
	if j.testSyncActivityDir != nil {
		return j.testSyncActivityDir(directory)
	}
	return syncDir(directory)
}
