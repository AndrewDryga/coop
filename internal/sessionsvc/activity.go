package sessionsvc

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/session"
)

// Activity narration turns the ACP frames a turn produces into durable session
// events, so a caller polling /v1/sessions/{id}/events can show what the model
// did instead of a stopwatch. Until this existed the whole interior of a turn —
// every tool call, every plan revision — was parsed one frame at a time and
// dropped, and the only visible trace of forty seconds of work was its total.
//
// Three properties are load-bearing:
//
// The frame loop never does I/O. The session store runs on a single SQLite
// connection, so an append issued inline would queue behind any other store
// operation, stall frame reading, back up the child's stdout pipe, and time out
// the very turn it was describing. observe() only parses and buffers; a drain
// goroutine owns every write.
//
// Every activity event is sequenced before the turn's terminal event. close()
// drains synchronously and runACP calls it before returning, which is before
// completeTurn appends turn.completed. A caller that stops polling at the
// terminal event would otherwise never see work sequenced after it.
//
// Narration failure is never turn failure. A dropped append costs a line in a
// timeline; a returned error would cost the answer. Drops are counted and
// reported as activity.elided rather than silently swallowed.
const (
	// sessionActivityMaxEvents bounds one turn's narration. A turn that trips
	// it keeps working and stops narrating, which is the right trade: the
	// answer matters more than the story of the answer.
	sessionActivityMaxEvents = 400
	// sessionActivityMaxTools bounds the in-flight call table. Finished calls
	// stay in it as tombstones — an agent may repeat a terminal update, and
	// forgetting a call would narrate it a second time from scratch — so the
	// table needs its own ceiling rather than shrinking as calls complete.
	sessionActivityMaxTools   = 512
	sessionActivityTitleBytes = 200
	// sessionActivityInputBytes bounds a recorded tool input. Arguments say
	// which action ran against which target, which is the difference between
	// "ran an Emisar action" and a fact an operator can check. Results are
	// excluded entirely: they dominate transcript size and routinely carry
	// credentials and log bodies into what is ultimately a browser page.
	sessionActivityInputBytes   = 2 << 10
	sessionActivityThoughtBytes = 4 << 10
	sessionActivityPlanEntries  = 32
	sessionActivityPlanBytes    = 300
)

// sessionActivityTool is a tool call in flight: what it was called, and whether
// its start has already been narrated. Titles arrive on the opening frame and a
// terminal update need not repeat them, so they are remembered per id.
type sessionActivityTool struct {
	title, kind string
	started     bool
	finished    bool
}

type sessionActivity struct {
	store     *session.Store
	sessionID string
	turnID    string

	mu      sync.Mutex
	pending []session.AppendEventRequest
	tools   map[string]*sessionActivityTool
	thought []byte
	budget  int
	dropped int
	closed  bool

	wake chan struct{}
	done chan struct{}
}

func newSessionActivity(store *session.Store, sessionID, turnID string) *sessionActivity {
	if store == nil || sessionID == "" {
		return nil
	}
	a := &sessionActivity{
		store: store, sessionID: sessionID, turnID: turnID,
		tools:  map[string]*sessionActivityTool{},
		budget: sessionActivityMaxEvents,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	go a.drain()
	return a
}

// observe records one ACP frame. It parses and buffers; it never writes and
// never blocks, so a frame loop can call it on every frame it reads.
func (a *sessionActivity) observe(raw json.RawMessage) {
	if a == nil || len(raw) == 0 {
		return
	}
	var envelope struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			ToolCallID    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Kind          string          `json:"kind"`
			Status        string          `json:"status"`
			RawInput      json.RawMessage `json:"rawInput"`
			Content       json.RawMessage `json:"content"`
			Entries       json.RawMessage `json:"entries"`
		} `json:"update"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	update := envelope.Update
	switch update.SessionUpdate {
	case "tool_call", "tool_call_update":
		a.observeTool(update.ToolCallID, update.Title, update.Kind, update.Status, update.RawInput)
	case "agent_thought_chunk", "assistant_thought_chunk":
		a.observeThought(update.Content)
	case "plan":
		a.observePlan(update.Entries)
	}
}

func (a *sessionActivity) observeTool(id, title, kind, status string, rawInput json.RawMessage) {
	if id == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tool := a.tools[id]
	if tool == nil {
		if len(a.tools) >= sessionActivityMaxTools {
			a.dropped++
			return
		}
		tool = &sessionActivityTool{}
		a.tools[id] = tool
	}
	if title != "" {
		tool.title = boundedActivityText(title, sessionActivityTitleBytes)
	}
	if kind != "" {
		tool.kind = boundedActivityText(kind, sessionActivityTitleBytes)
	}
	if !tool.started {
		tool.started = true
		// A thought that preceded an action belongs before it, so the story
		// reads "considered X, then did Y" instead of collapsing into one
		// undifferentiated block at the end of the turn.
		a.flushThoughtLocked()
		a.enqueueLocked(session.EventToolStarted, map[string]any{
			"tool_call_id": id,
			"title":        tool.title,
			"kind":         tool.kind,
			"input":        boundedActivityInput(rawInput),
		})
	}
	switch status {
	case "completed", "failed", "cancelled":
	default:
		return
	}
	if tool.finished {
		return
	}
	// Kept, not deleted. An agent that repeats a terminal update would
	// otherwise be handed a fresh entry and narrate the whole call again.
	tool.finished = true
	a.enqueueLocked(session.EventToolCompleted, map[string]any{
		"tool_call_id": id,
		"title":        tool.title,
		"kind":         tool.kind,
		"status":       status,
	})
}

func (a *sessionActivity) observeThought(content json.RawMessage) {
	text := activityContentText(content)
	if text == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.thought = append(a.thought, text...)
	if len(a.thought) >= sessionActivityThoughtBytes {
		a.flushThoughtLocked()
	}
}

func (a *sessionActivity) observePlan(entries json.RawMessage) {
	if len(entries) == 0 {
		return
	}
	var parsed []struct {
		Content  string `json:"content"`
		Status   string `json:"status"`
		Priority string `json:"priority"`
	}
	if json.Unmarshal(entries, &parsed) != nil || len(parsed) == 0 {
		return
	}
	if len(parsed) > sessionActivityPlanEntries {
		parsed = parsed[:sessionActivityPlanEntries]
	}
	steps := make([]map[string]string, 0, len(parsed))
	for _, entry := range parsed {
		content := boundedActivityText(entry.Content, sessionActivityPlanBytes)
		if content == "" {
			continue
		}
		steps = append(steps, map[string]string{
			"content":  content,
			"status":   boundedActivityText(entry.Status, 32),
			"priority": boundedActivityText(entry.Priority, 32),
		})
	}
	if len(steps) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.flushThoughtLocked()
	a.enqueueLocked(session.EventModelPlan, map[string]any{"entries": steps})
}

// permission records which way Coop answered a session/request_permission. The
// agent asked to do something and nobody human was there to answer; what the
// policy chose on their behalf is exactly the kind of fact a trace owes.
func (a *sessionActivity) permission(toolCallID, outcome, optionID, optionKind string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	title := ""
	if tool := a.tools[toolCallID]; tool != nil {
		title = tool.title
	}
	a.enqueueLocked(session.EventPermission, map[string]any{
		"tool_call_id": boundedActivityText(toolCallID, session.MaxIDBytes),
		"title":        title,
		"outcome":      boundedActivityText(outcome, 32),
		"option_id":    boundedActivityText(optionID, session.MaxIDBytes),
		"option_kind":  boundedActivityText(optionKind, 32),
	})
}

// close stops the drain and writes whatever is still buffered, synchronously.
// Callers must invoke it before the turn reaches its terminal event.
func (a *sessionActivity) close(ctx context.Context) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	// Flushed before the gate shuts, not after: enqueueLocked refuses a closed
	// recorder, so setting the flag first silently discarded the reasoning a
	// turn ended on and counted it as elided.
	a.flushThoughtLocked()
	a.closed = true
	dropped := a.dropped
	a.mu.Unlock()

	close(a.wake)
	<-a.done

	if dropped > 0 {
		a.mu.Lock()
		// The marker is the last thing written and is exempt from the budget:
		// a narration that ran out of room has to say so, or the timeline
		// reads as a complete account of a turn it only partly saw.
		a.pending = append(a.pending, a.request(session.EventActivityElided, map[string]any{
			"dropped": dropped,
			"reason":  "the turn produced more activity than one turn may narrate",
		}))
		a.mu.Unlock()
	}
	a.write(ctx)
}

func (a *sessionActivity) drain() {
	defer close(a.done)
	for range a.wake {
		a.write(context.Background())
	}
}

// write moves everything currently buffered into the store. Appends happen
// outside the lock so observe() stays non-blocking while the store is busy.
func (a *sessionActivity) write(ctx context.Context) {
	for {
		a.mu.Lock()
		batch := a.pending
		a.pending = nil
		a.mu.Unlock()
		if len(batch) == 0 {
			return
		}
		for _, request := range batch {
			// Best effort by construction: a session that closed underneath us
			// or a store that refused the row must not disturb the turn.
			_, _ = a.store.AppendEvent(ctx, request)
		}
	}
}

// enqueueLocked buffers one event. The per-turn budget is the only thing that
// drops: it already bounds how much may ever be queued, so a separate
// queue-depth limit would add nothing but a second, indistinguishable reason
// for a gap in the story — and would fire on a fast burst the drain would have
// caught up with anyway.
func (a *sessionActivity) enqueueLocked(eventType session.EventType, payload map[string]any) {
	if a.closed || a.budget <= 0 {
		a.dropped++
		return
	}
	a.budget--
	a.pending = append(a.pending, a.request(eventType, payload))
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *sessionActivity) request(eventType session.EventType, payload map[string]any) session.AppendEventRequest {
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > session.MaxEventPayloadBytes {
		encoded = []byte(`{}`)
	}
	return session.AppendEventRequest{
		SessionID: a.sessionID, TurnID: a.turnID, Type: eventType, Version: 1, Payload: encoded,
	}
}

func (a *sessionActivity) flushThoughtLocked() {
	text := strings.TrimSpace(string(a.thought))
	a.thought = nil
	if text == "" {
		return
	}
	a.enqueueLocked(session.EventModelThought, map[string]any{
		"text": boundedActivityText(text, sessionActivityThoughtBytes),
	})
}

func activityContentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var parsed struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parsed) != nil || parsed.Type != "text" {
		return ""
	}
	return parsed.Text
}

// boundedActivityInput keeps a tool's arguments only when they are small
// enough to be a label rather than a payload. An oversized input is dropped
// whole instead of cut, because half a JSON object is not evidence.
func boundedActivityInput(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || len(raw) > sessionActivityInputBytes || !json.Valid(raw) {
		return nil
	}
	return raw
}

func boundedActivityText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.ReplaceAll(value, "\x00", "�")
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
}
