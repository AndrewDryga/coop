package workerconnector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

const (
	eventStreamVersion = 1
	maximumEventPage   = 100
	maximumPollEvents  = 100
	maximumPollBytes   = 512 << 10
	maximumPlanSteps   = 32
	maximumEventDelay  = time.Second
)

type sessionEventAPI interface {
	ListEvents(context.Context, string, int64, int) ([]workerproto.SessionEvent, error)
}

type eventStream struct {
	Version               int    `json:"version"`
	SessionRef            string `json:"session_ref"`
	PlacementGeneration   int    `json:"placement_generation"`
	CoopSessionID         string `json:"coop_session_id"`
	AcknowledgedSequence  int64  `json:"acknowledged_sequence"`
	LastPublishedSequence int64  `json:"last_published_sequence"`
	TerminalSequence      int64  `json:"terminal_sequence"`
}

func (j *journal) bindEventStream(command workerproto.Command, coopSessionID string) error {
	path := j.eventStreamPath(command.SessionRef)
	current, err := j.readEventStream(path)
	if err == nil {
		switch {
		case current.PlacementGeneration > command.PlacementGeneration:
			return j.syncActivityDir(j.streams)
		case current.PlacementGeneration == command.PlacementGeneration && current.CoopSessionID == coopSessionID:
			return j.syncActivityDir(j.streams)
		case current.PlacementGeneration == command.PlacementGeneration:
			return errors.New("worker event stream session identity conflicts")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return j.writeEventStream(path, eventStream{
		Version: eventStreamVersion, SessionRef: command.SessionRef,
		PlacementGeneration: command.PlacementGeneration, CoopSessionID: coopSessionID,
	})
}

func (j *journal) eventStreams() ([]eventStream, error) {
	entries, err := os.ReadDir(j.streams)
	if err != nil {
		return nil, fmt.Errorf("read worker event stream directory: %w", err)
	}
	streams := make([]eventStream, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		stream, err := j.readEventStream(filepath.Join(j.streams, entry.Name()))
		if err != nil {
			return nil, err
		}
		streams = append(streams, stream)
	}
	sort.Slice(streams, func(left, right int) bool {
		return streams[left].SessionRef < streams[right].SessionRef
	})
	if cursor, err := os.ReadFile(j.eventScanPath()); err == nil {
		after := string(cursor)
		index := sort.Search(len(streams), func(index int) bool { return streams[index].SessionRef > after })
		streams = append(streams[index:], streams[:index]...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read worker event scan cursor: %w", err)
	}
	return streams, nil
}

func (j *journal) advanceEventScan(sessionRef string) error {
	if !reference(sessionRef, 256) {
		return errors.New("worker event scan cursor is malformed")
	}
	path := j.eventScanPath()
	temporary, err := os.CreateTemp(j.streams, ".event-scan-*")
	if err != nil {
		return fmt.Errorf("create worker event scan cursor: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(sessionRef); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDir(j.streams)
}

func (j *journal) publishEventStream(stream eventStream, sequence, terminalSequence int64) error {
	return j.withActivityLock(func() error { return j.publishEventStreamLocked(stream, sequence, terminalSequence) })
}

func (j *journal) publishEventStreamLocked(stream eventStream, sequence, terminalSequence int64) error {
	if !j.eventStreamAuthorized(stream) {
		return errors.New("worker event stream origin is not bound")
	}
	current, err := j.readEventStream(j.eventStreamPath(stream.SessionRef))
	if err != nil {
		return err
	}
	if current.PlacementGeneration != stream.PlacementGeneration || current.CoopSessionID != stream.CoopSessionID {
		return errors.New("worker event stream changed before publication")
	}
	if sequence < current.AcknowledgedSequence {
		return errors.New("worker event publication precedes its acknowledgement")
	}
	if sequence > current.LastPublishedSequence {
		current.LastPublishedSequence = sequence
	}
	if terminalSequence > 0 {
		current.TerminalSequence = terminalSequence
	}
	return j.writeEventStream(j.eventStreamPath(stream.SessionRef), current)
}

func (j *journal) acknowledgeEvents(acknowledgements []workerproto.EventAcknowledgement) error {
	if len(acknowledgements) == 0 {
		return nil
	}
	return j.withActivityLock(func() error { return j.acknowledgeEventsLocked(acknowledgements) })
}

func (j *journal) acknowledgeEventsLocked(acknowledgements []workerproto.EventAcknowledgement) error {
	for _, acknowledgement := range acknowledgements {
		path := j.eventStreamPath(acknowledgement.SessionRef)
		stream, err := j.readEventStream(path)
		if err != nil {
			return err
		}
		if stream.PlacementGeneration != acknowledgement.PlacementGeneration ||
			!j.eventStreamAuthorized(stream) ||
			acknowledgement.Sequence < stream.AcknowledgedSequence ||
			acknowledgement.Sequence > stream.LastPublishedSequence {
			return errors.New("worker event acknowledgement conflicts with published custody")
		}
		stream.AcknowledgedSequence = acknowledgement.Sequence
		if stream.TerminalSequence > 0 && acknowledgement.Sequence >= stream.TerminalSequence {
			origin, err := j.readCreateOrigin(j.createOriginPath(stream.SessionRef))
			if errors.Is(err, os.ErrNotExist) || (err == nil && origin.PlacementGeneration < stream.PlacementGeneration) {
				// A legacy stream is still the only proof of this generation. Keep its
				// acknowledged terminal cursor as a dormant floor instead of inventing an origin.
				if err := j.writeEventStream(path, stream); err != nil {
					return err
				}
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove terminal worker event stream: %w", err)
			}
			if err := j.syncActivityDir(j.streams); err != nil {
				return err
			}
			continue
		}
		if err := j.writeEventStream(path, stream); err != nil {
			return err
		}
	}
	return nil
}

func (j *journal) readEventStream(path string) (eventStream, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return eventStream{}, err
	}
	var stream eventStream
	if err := json.Unmarshal(data, &stream); err != nil {
		return eventStream{}, fmt.Errorf("decode worker event stream: %w", err)
	}
	if stream.Version != eventStreamVersion || !reference(stream.SessionRef, 256) ||
		stream.PlacementGeneration <= 0 || !reference(stream.CoopSessionID, 1024) ||
		stream.AcknowledgedSequence < 0 || stream.LastPublishedSequence < stream.AcknowledgedSequence ||
		stream.TerminalSequence < 0 || stream.TerminalSequence > stream.LastPublishedSequence {
		return eventStream{}, errors.New("worker event stream is malformed")
	}
	return stream, nil
}

func (j *journal) writeEventStream(path string, stream eventStream) error {
	return j.writeActivityRecord(path, stream)
}

func (j *journal) writeActivityRecord(path string, record any) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode worker activity record: %w", err)
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".activity-record-*")
	if err != nil {
		return fmt.Errorf("create worker activity record: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect worker activity record: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write worker activity record: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync worker activity record: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close worker activity record: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish worker activity record: %w", err)
	}
	return j.syncActivityDir(directory)
}

func (j *journal) eventStreamPath(sessionRef string) string {
	digest := sha256.Sum256([]byte(sessionRef))
	return filepath.Join(j.streams, hex.EncodeToString(digest[:])+".json")
}

func (j *journal) eventScanPath() string { return filepath.Join(j.streams, ".scan-cursor") }

func (e *Executor) collectActivity(ctx context.Context, maximumBytes int) ([]workerproto.EventBatch, error) {
	api, ok := e.api.(sessionEventAPI)
	if !ok || maximumBytes <= 0 {
		return []workerproto.EventBatch{}, nil
	}
	collectionCtx, cancel := context.WithTimeout(ctx, maximumEventDelay)
	defer cancel()
	targets, issues := e.activityTargets()
	if targets == nil && issues != nil {
		return []workerproto.EventBatch{}, issues
	}
	batches := make([]workerproto.EventBatch, 0, len(targets))
	remainingEvents, remainingBytes := maximumPollEvents, min(maximumPollBytes, maximumBytes)
	lastScanned := ""
	for index, target := range targets {
		if collectionCtx.Err() != nil {
			break
		}
		if index == maximumEventPage || remainingEvents == 0 || remainingBytes == 0 {
			break
		}
		lastScanned = target.sessionRef
		if target.origin != nil {
			if err := e.resolveCreateOrigin(collectionCtx, *target.origin); err != nil {
				issues = errors.Join(issues, fmt.Errorf("resolve worker create %s: %w", target.sessionRef, err))
			}
			continue
		}
		stream := *target.stream
		limit := min(maximumEventPage, remainingEvents)
		events, err := api.ListEvents(collectionCtx, stream.CoopSessionID, stream.AcknowledgedSequence, limit)
		if err != nil || len(events) == 0 || len(events) > limit {
			issues = errors.Join(issues, err)
			continue
		}
		batchEvents := make([]workerproto.Event, 0, len(events))
		batchBytes := 0
		terminalSequence := stream.TerminalSequence
		for _, event := range events {
			if event.SessionID != stream.CoopSessionID || event.Sequence != stream.AcknowledgedSequence+int64(len(batchEvents))+1 || event.Validate() != nil {
				batchEvents = nil
				break
			}
			if operatorActivityEvent(event.Type) {
				payload, ok := publicActivityPayload(event.Type, event.Payload)
				if !ok {
					batchEvents = nil
					break
				}
				event.Payload = payload
			} else {
				event.Payload = nil
			}
			payload, err := json.Marshal(event)
			if err != nil || batchBytes+len(payload) > remainingBytes {
				break
			}
			batchEvents = append(batchEvents, workerproto.Event{
				Sequence: event.Sequence, Kind: workerproto.SessionEventKind, Payload: payload,
			})
			if terminalSessionEvent(event.Type) {
				terminalSequence = event.Sequence
			}
			batchBytes += len(payload)
		}
		if len(batchEvents) == 0 {
			continue
		}
		lastSequence := batchEvents[len(batchEvents)-1].Sequence
		if err := e.journal.publishEventStream(stream, lastSequence, terminalSequence); err != nil {
			issues = errors.Join(issues, err)
			continue
		}
		batches = append(batches, workerproto.EventBatch{
			SessionRef: stream.SessionRef, PlacementGeneration: stream.PlacementGeneration,
			AfterSequence: stream.AcknowledgedSequence, Events: batchEvents,
		})
		remainingEvents -= len(batchEvents)
		remainingBytes -= batchBytes
	}
	if lastScanned != "" {
		issues = errors.Join(issues, e.journal.advanceEventScan(lastScanned))
	}
	return batches, issues
}

func operatorActivityEvent(kind string) bool {
	switch kind {
	case "tool.started", "tool.completed", "model.plan", "model.thought", "permission.decided", "activity.elided", "provider.backoff", "provider.alive":
		return true
	default:
		return false
	}
}

func terminalSessionEvent(kind string) bool {
	return kind == "workspace.discarded"
}

// publicActivityPayload is the privacy boundary between a local Coop session
// transcript and Responder's durable operator trace. Free-form thought, plan,
// title, reason, command, and tool argument bytes never cross it.
func publicActivityPayload(kind string, raw json.RawMessage) (json.RawMessage, bool) {
	var value map[string]any
	if len(raw) > 0 && json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	if value == nil {
		value = map[string]any{}
	}
	public := map[string]any{}
	switch kind {
	case "tool.started":
		copyPublicText(public, "tool_call_id", value["tool_call_id"], 1024)
		if input, ok := value["input"].(map[string]any); ok {
			visible := map[string]any{}
			copyPublicText(visible, "server", input["server"], 128)
			copyPublicText(visible, "tool", input["tool"], 128)
			if arguments, ok := input["arguments"].(map[string]any); ok {
				copyPublicText(visible, "operation", arguments["action_id"], 128)
			}
			if _, ok := visible["operation"]; !ok {
				copyPublicText(visible, "operation", input["action_id"], 128)
			}
			if _, ok := visible["operation"]; !ok {
				copyPublicText(visible, "operation", input["operation"], 128)
			}
			if len(visible) > 0 {
				public["input"] = visible
			}
		}
	case "tool.completed":
		copyPublicText(public, "tool_call_id", value["tool_call_id"], 1024)
		copyEnum(public, "status", value["status"], "completed", "failed", "cancelled")
	case "model.plan":
		if entries, ok := value["entries"].([]any); ok {
			public["step_count"] = min(len(entries), maximumPlanSteps)
		}
	case "model.thought":
		// Presence and time are useful; free-form private reasoning is not.
	case "permission.decided":
		copyPublicText(public, "tool_call_id", value["tool_call_id"], 1024)
		copyEnum(public, "outcome", value["outcome"], "selected", "allowed", "denied", "cancelled")
		copyPublicText(public, "option_kind", value["option_kind"], 32)
	case "activity.elided":
		copyPublicInteger(public, "dropped", value["dropped"])
	case "provider.backoff":
		copyPublicInteger(public, "attempt", value["attempt"])
		copyPublicInteger(public, "retry_after_seconds", value["retry_after_seconds"])
		copyPublicText(public, "target", value["target"], 256)
		copyPublicText(public, "next_target", value["next_target"], 256)
		copyPublicTime(public, "reset_at", value["reset_at"])
		copyPublicTime(public, "all_limited_until", value["all_limited_until"])
	case "provider.alive":
		copyPublicInteger(public, "frames", value["frames"])
		copyPublicInteger(public, "bytes", value["bytes"])
	default:
		return nil, false
	}
	encoded, err := json.Marshal(public)
	return encoded, err == nil
}

func copyPublicText(target map[string]any, key string, raw any, maximum int) {
	value, ok := raw.(string)
	if !ok || value == "" || len(value) > maximum || strings.ContainsRune(value, '\x00') {
		return
	}
	target[key] = strings.ToValidUTF8(value, "�")
}

func copyEnum(target map[string]any, key string, raw any, allowed ...string) {
	value, ok := raw.(string)
	if !ok {
		return
	}
	for _, candidate := range allowed {
		if value == candidate {
			target[key] = value
			return
		}
	}
}

func copyPublicInteger(target map[string]any, key string, raw any) {
	value, ok := raw.(float64)
	if ok && value >= 0 && value <= 1<<53 && value == float64(int64(value)) {
		target[key] = int64(value)
	}
}

func copyPublicTime(target map[string]any, key string, raw any) {
	value, ok := raw.(string)
	if !ok {
		return
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err == nil {
		target[key] = parsed.UTC().Format(time.RFC3339)
	}
}

func eventBatchBudget(base workerproto.Poll) int {
	base.EventBatches = []workerproto.EventBatch{}
	encoded, err := encodeWireJSON(base)
	if err != nil || len(encoded) >= workerproto.MaxDocumentBytes {
		return 0
	}
	// The payload count excludes event/batch JSON framing. Reserve enough for
	// the maximum 100 small envelopes and the document terminator.
	return max(workerproto.MaxDocumentBytes-len(encoded)-(64<<10), 0)
}

func pollFits(poll workerproto.Poll) bool {
	encoded, err := encodeWireJSON(poll)
	return err == nil && len(encoded) <= workerproto.MaxDocumentBytes
}
