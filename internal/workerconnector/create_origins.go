package workerconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

const createOriginVersion = 1

var errCreateOriginConflict = errors.New("worker create origin identity conflicts")

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
	if entry.Command == nil || entry.Result == nil || !isCreateRequest(*entry.Command) || entry.Result.State != "succeeded" {
		return nil
	}
	return j.withActivityLock(func() error { return j.preserveCreateOriginLocked(entry) })
}

func createOriginFromReceipt(entry journalEntry) (createOrigin, error) {
	command, result := entry.Command, entry.Result
	computed, err := commandDigest(*command)
	if err != nil || computed != entry.CommandDigest || command.Validate() != nil || result.Validate() != nil ||
		entry.CommandID != command.CommandID || result.CommandID != command.CommandID || result.OperationKey != command.IdempotencyKey {
		return createOrigin{}, errors.New("worker create receipt identity is malformed")
	}
	var resource struct {
		Operation createOperation `json:"operation"`
		Session   struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	var response workerproto.APIResponse
	if err := json.Unmarshal(result.Resource, &response); err != nil || response.Status < 200 || response.Status >= 300 {
		return createOrigin{}, nil
	}
	if err := json.Unmarshal(response.Body, &resource); err != nil {
		return createOrigin{}, errors.New("worker create receipt resource is malformed")
	}
	op := resource.Operation
	// Older synchronous receipts proved session.id directly. Async receipts must prove the
	// accepted create operation instead; a later get_session/reconcile payload is never authority.
	if resource.Session.ID != "" {
		if !reference(resource.Session.ID, 1024) || (op.Method != "" && op.Method != "CreateRemoteSession") ||
			(op.ID != "" && !reference(op.ID, 256)) || (op.State != "" && op.State != "succeeded") ||
			(op.ResourceType != "" && op.ResourceType != "session") ||
			(op.ResourceID != "" && op.ResourceID != resource.Session.ID) {
			return createOrigin{}, errors.New("worker synchronous create identity conflicts")
		}
	} else if !reference(op.ID, 256) || op.Method != "CreateRemoteSession" || !createOperationState(op.State) {
		return createOrigin{}, errors.New("worker create receipt has no accepted operation identity")
	}
	origin := createOrigin{
		Version: createOriginVersion, WorkerID: command.WorkerID, SessionRef: command.SessionRef, PlacementGeneration: command.PlacementGeneration,
		CommandID: command.CommandID, CommandDigest: computed, OperationKey: command.IdempotencyKey,
		OperationID: op.ID, State: "pending", CoopSessionID: resource.Session.ID,
	}
	return origin, nil
}

func (j *journal) preserveCreateOriginLocked(entry journalEntry) error {
	origin, err := createOriginFromReceipt(entry)
	if err != nil || origin.CommandID == "" {
		return err
	}
	current, err := j.loadCreateOrigin(origin.SessionRef)
	if err == nil {
		if current.PlacementGeneration > origin.PlacementGeneration {
			return nil
		}
		if current.PlacementGeneration == origin.PlacementGeneration {
			if !sameCreateOrigin(current, origin) || (origin.CoopSessionID != "" && current.CoopSessionID != "" && origin.CoopSessionID != current.CoopSessionID) {
				return errCreateOriginConflict
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
			return nil
		}
		if stream.PlacementGeneration == origin.PlacementGeneration && origin.CoopSessionID != "" && stream.CoopSessionID != origin.CoopSessionID {
			return errors.New("worker create origin conflicts with existing stream")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := j.writeCreateOriginLocked(origin); err != nil {
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

// A create origin lives in one of two folders. A pending one stays in create-origins/, which the
// activity scan lists every poll; once it settles (bound or failed) it moves to
// create-origins/settled/, which no poll lists. A settled origin is still the generation watermark,
// so every reader goes through loadCreateOrigin, which looks in both.
//
// Coop builds before this layout keep every origin in create-origins/ and never look in settled/:
// such a build would not see a settled origin, so its watermark would not stop an old command from
// bringing back a replaced or discarded session. A worker must not go back to one.
func (j *journal) createOriginPath(sessionRef string) string {
	return filepath.Join(j.origins, filepath.Base(j.eventStreamPath(sessionRef)))
}

func (j *journal) settledCreateOriginPath(sessionRef string) string {
	return filepath.Join(j.settled, filepath.Base(j.eventStreamPath(sessionRef)))
}

// loadCreateOrigin reads the origin that governs a session. Both folders can hold one: a crash
// between writing a settled origin and removing its pending file leaves both, and a newer
// generation's pending create sits beside the older generation's settled one. The higher
// generation governs; at the same generation the settled one does, since a create only settles.
func (j *journal) loadCreateOrigin(sessionRef string) (createOrigin, error) {
	pending, pendingErr := j.readCreateOrigin(j.createOriginPath(sessionRef))
	settled, settledErr := j.readCreateOrigin(j.settledCreateOriginPath(sessionRef))
	switch {
	case pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist):
		return createOrigin{}, pendingErr
	case settledErr != nil && !errors.Is(settledErr, os.ErrNotExist):
		return createOrigin{}, settledErr
	case pendingErr != nil:
		return settled, settledErr
	case settledErr != nil:
		return pending, nil
	}
	return governingCreateOrigin(pending, settled), nil
}

func governingCreateOrigin(pending, settled createOrigin) createOrigin {
	if pending.PlacementGeneration > settled.PlacementGeneration {
		return pending
	}
	return settled
}

// writeCreateOriginLocked publishes an origin in the folder its state belongs to. Settling writes
// the settled file first and only then removes the pending one, so a crash in between leaves both
// (loadCreateOrigin reads the settled one) and never neither.
func (j *journal) writeCreateOriginLocked(origin createOrigin) error {
	if origin.State == "pending" {
		return j.writeActivityRecord(j.createOriginPath(origin.SessionRef), origin)
	}
	if err := j.writeActivityRecord(j.settledCreateOriginPath(origin.SessionRef), origin); err != nil {
		return err
	}
	if err := os.Remove(j.createOriginPath(origin.SessionRef)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove settled worker create origin: %w", err)
	}
	return nil
}

// moveSettledCreateOrigins moves the settled origins a build before the settled/ folder left in
// create-origins/, once, when the journal opens. Each move is a rename within one folder tree, so a
// crash leaves a record in exactly one place and the next start finishes the rest. A file its
// settled twin already governs (a pending file left by a crash while settling, an exact copy, an
// older generation) is removed; a damaged file stays where the scan reports it.
func (j *journal) moveSettledCreateOrigins() error {
	return j.withActivityLock(func() error {
		entries, err := os.ReadDir(j.origins)
		if err != nil {
			return fmt.Errorf("list worker create origins: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			path, settledPath := filepath.Join(j.origins, entry.Name()), filepath.Join(j.settled, entry.Name())
			origin, err := j.readCreateOrigin(path)
			if err != nil {
				continue
			}
			settled, err := j.readCreateOrigin(settledPath)
			switch {
			case err != nil && !errors.Is(err, os.ErrNotExist):
				continue
			case err == nil && origin.PlacementGeneration <= settled.PlacementGeneration:
				if origin.State != "pending" && origin.PlacementGeneration == settled.PlacementGeneration && origin != settled {
					continue // two different settled records of one generation: keep both, settled/ governs
				}
				err = os.Remove(path)
			case origin.State == "pending":
				continue
			default:
				err = os.Rename(path, settledPath)
			}
			if err != nil {
				return fmt.Errorf("move settled worker create origin %s: %w", entry.Name(), err)
			}
		}
		return nil
	})
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
	if len(data) > 16<<10 || json.Unmarshal(data, &origin) != nil || origin.Version != createOriginVersion ||
		!reference(origin.WorkerID, 256) || !reference(origin.SessionRef, 256) || origin.PlacementGeneration <= 0 ||
		!reference(origin.CommandID, 256) || !digest(origin.CommandDigest) || !reference(origin.OperationKey, 512) ||
		(origin.State != "pending" && origin.State != "bound" && origin.State != "failed") ||
		(origin.OperationID == "" && origin.CoopSessionID == "") ||
		(origin.OperationID != "" && !reference(origin.OperationID, 256)) ||
		(origin.CoopSessionID != "" && !reference(origin.CoopSessionID, 1024)) ||
		(origin.State == "bound" && origin.CoopSessionID == "") ||
		(path != j.createOriginPath(origin.SessionRef) && (path != j.settledCreateOriginPath(origin.SessionRef) || origin.State == "pending")) {
		return createOrigin{}, errors.New("worker create origin is malformed")
	}
	return origin, nil
}

func (j *journal) bindCreateOrigin(origin createOrigin, sessionID string) error {
	return j.withActivityLock(func() error { return j.bindCreateOriginLocked(origin, sessionID) })
}

func (j *journal) bindCreateOriginLocked(origin createOrigin, sessionID string) error {
	current, err := j.loadCreateOrigin(origin.SessionRef)
	if err != nil {
		return err
	}
	if !sameCreateOrigin(current, origin) {
		return errors.New("worker create origin changed before binding")
	}
	if current.State != "pending" {
		return nil
	}
	if !reference(sessionID, 1024) || (current.CoopSessionID != "" && current.CoopSessionID != sessionID) {
		return errors.New("worker create session identity conflicts")
	}
	if err := j.bindEventStream(workerproto.Command{SessionRef: origin.SessionRef, PlacementGeneration: origin.PlacementGeneration}, sessionID); err != nil {
		return err
	}
	// A pending origin suppresses the stream until both are published. On retry,
	// bindEventStream adopts the exact existing stream without resetting its published cursors.
	current.State, current.CoopSessionID = "bound", sessionID
	return j.writeCreateOriginLocked(current)
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
			current, err := e.journal.loadCreateOrigin(origin.SessionRef)
			if err != nil || !sameCreateOrigin(current, origin) || current.State != "pending" {
				return errors.New("worker create origin changed before failure")
			}
			current.State = "failed"
			return e.journal.writeCreateOriginLocked(current)
		})
	case "uncertain":
		return errCreateOperationUncertain
	}
	return nil
}

// errCreateOperationUncertain is not a failure: the daemon has not settled the create yet and may
// take long to. Its lookups back off like failed ones, without a warning; a reserved or running
// create is still looked up every poll, so a new session's narration starts at once.
var errCreateOperationUncertain = errors.New("worker create operation is uncertain")

type activityTarget struct {
	sessionRef string
	origin     *createOrigin
	stream     *eventStream
}

// activityTargets lists the pending origins and the event streams, never the settled folder: a
// stream's settled origin is looked up by its name, so a settled session whose stream is gone
// costs a poll nothing.
func (e *Executor) activityTargets() ([]activityTarget, error) {
	streams, err := e.journal.eventStreams()
	if err != nil {
		return nil, err
	}
	targets := make(map[string]activityTarget, len(streams))
	streamNames := make(map[string]string, len(streams))
	for _, stream := range streams {
		targets[stream.SessionRef] = activityTarget{sessionRef: stream.SessionRef, stream: &stream}
		streamNames[filepath.Base(e.journal.createOriginPath(stream.SessionRef))] = stream.SessionRef
	}
	files, err := os.ReadDir(e.journal.origins)
	if err != nil {
		return nil, err
	}
	var issues error
	read := make(map[string]bool, len(files)+len(streams))
	apply := func(name string, origin createOrigin, err error) {
		read[name] = true
		if errors.Is(err, os.ErrNotExist) {
			return // a stream from before origins is its own generation floor
		}
		if err != nil {
			issues = errors.Join(issues, fmt.Errorf("worker create origin %s: %w", name, err))
			// The filename, not untrusted decoded identity, determines which legacy
			// stream must be suppressed. Healthy siblings can still narrate their work.
			if ref, ok := streamNames[name]; ok {
				delete(targets, ref)
			}
			return
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
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		origin, err := e.journal.readCreateOrigin(filepath.Join(e.journal.origins, file.Name()))
		if err == nil {
			// A pending file a crash left beside its settled twin yields to it.
			settled, settledErr := e.readSettledCreateOrigin(file.Name())
			if settledErr == nil {
				origin = governingCreateOrigin(origin, settled)
			} else if !errors.Is(settledErr, os.ErrNotExist) {
				err = settledErr
			}
		}
		apply(file.Name(), origin, err)
	}
	for name := range streamNames {
		if !read[name] {
			origin, err := e.readSettledCreateOrigin(name)
			apply(name, origin, err)
		}
	}
	for name := range e.settledOrigins {
		if !read[name] {
			delete(e.settledOrigins, name)
		}
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

// settledCreateOrigin is a settled origin as this process last read it, with the file it read it
// from. A rewrite creates its new file before the old one is removed, so it never takes the old
// inode; only a third write within the same timestamp tick could.
type settledCreateOrigin struct {
	file   fs.FileInfo
	origin createOrigin
}

// readSettledCreateOrigin reads the settled origin of a live stream or a pending file. One this
// process already read comes from memory while its file is the same one: every origin write
// replaces the file whole (WriteFileAtomic), so the same inode, size and modification time is the
// same record, and a poll costs a stat, not an open.
func (e *Executor) readSettledCreateOrigin(name string) (createOrigin, error) {
	path := filepath.Join(e.journal.settled, name)
	info, err := os.Lstat(path)
	if err != nil {
		delete(e.settledOrigins, name)
		return createOrigin{}, err
	}
	if cached, ok := e.settledOrigins[name]; ok && os.SameFile(cached.file, info) &&
		cached.file.Size() == info.Size() && cached.file.ModTime().Equal(info.ModTime()) {
		return cached.origin, nil
	}
	delete(e.settledOrigins, name)
	origin, err := e.journal.readCreateOrigin(path)
	if err == nil {
		if e.settledOrigins == nil {
			e.settledOrigins = make(map[string]settledCreateOrigin)
		}
		e.settledOrigins[name] = settledCreateOrigin{file: info, origin: origin}
	}
	return origin, err
}

func (j *journal) eventStreamAuthorized(stream eventStream) bool {
	origin, err := j.loadCreateOrigin(stream.SessionRef)
	return errors.Is(err, os.ErrNotExist) || (err == nil && (origin.PlacementGeneration < stream.PlacementGeneration ||
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
