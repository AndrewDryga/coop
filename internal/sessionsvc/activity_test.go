package sessionsvc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
)

func newActivityTestStore(t *testing.T) (*session.Store, session.Session) {
	t.Helper()
	store, err := session.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sess, err := store.CreateSession(context.Background(), "create", session.CreateSessionRequest{
		Target: "codex@work", Policy: "policy", Repository: t.TempDir(),
		Workspace: t.TempDir(), ForkName: "fork", BaseCommit: strings.Repeat("a", 40),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, sess
}

func activityEvents(t *testing.T, store *session.Store, sessionID string) []session.Event {
	t.Helper()
	events, err := store.ListEvents(context.Background(), sessionID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	recorded := make([]session.Event, 0, len(events))
	for _, event := range events {
		switch event.Type {
		case session.EventToolStarted, session.EventToolCompleted, session.EventModelPlan,
			session.EventModelThought, session.EventPermission, session.EventActivityElided,
			session.EventProviderAlive:
			recorded = append(recorded, event)
		}
	}
	return recorded
}

func activityPayload(t *testing.T, event session.Event) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode %s payload: %v", event.Type, err)
	}
	return payload
}

// A tool call is narrated once at its start and once when it reaches a
// terminal status, carrying the title from the opening frame even though the
// terminal update does not repeat it.
func TestSessionActivityNarratesToolCall(t *testing.T) {
	store, sess := newActivityTestStore(t)
	activity := newSessionActivity(store, sess.ID, "turn-1")

	activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call","toolCallId":"t1",` +
		`"title":"Run Emisar action nomad.job_status","kind":"execute","status":"pending",` +
		`"rawInput":{"action":"nomad.job_status","job":"website"}}}`))
	activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"in_progress"}}`))
	activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}`))
	activity.close(context.Background())

	events := activityEvents(t, store, sess.ID)
	if len(events) != 2 {
		t.Fatalf("want a start and a completion, got %d events", len(events))
	}
	if events[0].Type != session.EventToolStarted || events[1].Type != session.EventToolCompleted {
		t.Fatalf("wrong order: %s then %s", events[0].Type, events[1].Type)
	}
	if events[0].Sequence >= events[1].Sequence {
		t.Fatalf("sequences are not increasing: %d then %d", events[0].Sequence, events[1].Sequence)
	}
	started := activityPayload(t, events[0])
	if started["title"] != "Run Emisar action nomad.job_status" || started["kind"] != "execute" {
		t.Fatalf("start lost its identity: %v", started)
	}
	if input, _ := started["input"].(map[string]any); input["job"] != "website" {
		t.Fatalf("start lost its bounded input: %v", started["input"])
	}
	completed := activityPayload(t, events[1])
	if completed["status"] != "completed" {
		t.Fatalf("completion lost its status: %v", completed)
	}
	// The terminal frame carried no title. Forgetting it here would print an
	// opaque id in a timeline that already knew the human-readable name.
	if completed["title"] != "Run Emisar action nomad.job_status" {
		t.Fatalf("completion lost the remembered title: %v", completed)
	}
	for _, event := range events {
		if event.TurnID != "turn-1" {
			t.Fatalf("event %s lost its turn: %q", event.Type, event.TurnID)
		}
	}
}

// Repeated non-terminal updates and a repeated terminal status must not each
// produce a row, or one tool call becomes a dozen timeline entries.
func TestSessionActivityEmitsEachToolCallOnce(t *testing.T) {
	store, sess := newActivityTestStore(t)
	activity := newSessionActivity(store, sess.ID, "turn-1")
	for range 5 {
		activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"in_progress"}}`))
	}
	for range 3 {
		activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"failed"}}`))
	}
	activity.close(context.Background())

	events := activityEvents(t, store, sess.ID)
	if len(events) != 2 {
		t.Fatalf("want exactly one start and one completion, got %d", len(events))
	}
	if activityPayload(t, events[1])["status"] != "failed" {
		t.Fatalf("kept the wrong terminal status")
	}
}

// Thought chunks arrive in many small pieces. One event per chunk would bury
// the trace, so they coalesce and flush when the model stops thinking and acts.
func TestSessionActivityCoalescesThoughtBeforeTheToolItPrecedes(t *testing.T) {
	store, sess := newActivityTestStore(t)
	activity := newSessionActivity(store, sess.ID, "turn-1")
	for _, chunk := range []string{"Check ", "the ", "rollout."} {
		activity.observe(json.RawMessage(
			`{"update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"` + chunk + `"}}}`))
	}
	activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Read job","kind":"read"}}`))
	activity.close(context.Background())

	events := activityEvents(t, store, sess.ID)
	if len(events) != 2 {
		t.Fatalf("want one coalesced thought and one tool start, got %d", len(events))
	}
	if events[0].Type != session.EventModelThought {
		t.Fatalf("the thought must precede the action it explains, got %s first", events[0].Type)
	}
	if got := activityPayload(t, events[0])["text"]; got != "Check the rollout." {
		t.Fatalf("chunks did not coalesce: %q", got)
	}
}

// A turn that ends on reasoning rather than on a tool call still said
// something. Closing must flush it, and must not count it as elided.
func TestSessionActivityFlushesTheThoughtATurnEndsOn(t *testing.T) {
	store, sess := newActivityTestStore(t)
	activity := newSessionActivity(store, sess.ID, "turn-1")
	activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Read","kind":"read","status":"completed"}}`))
	activity.observe(json.RawMessage(
		`{"update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"That confirms the rollout."}}}`))
	activity.close(context.Background())

	events := activityEvents(t, store, sess.ID)
	if len(events) != 3 {
		t.Fatalf("want a start, a completion and the closing thought, got %d: %v",
			len(events), eventTypes(events))
	}
	last := events[len(events)-1]
	if last.Type != session.EventModelThought {
		t.Fatalf("the closing thought was lost; last event is %s", last.Type)
	}
	if got := activityPayload(t, last)["text"]; got != "That confirms the rollout." {
		t.Fatalf("closing thought = %q", got)
	}
	for _, event := range events {
		if event.Type == session.EventActivityElided {
			t.Fatal("a flushed thought was counted as elided")
		}
	}
}

func eventTypes(events []session.Event) []session.EventType {
	types := make([]session.EventType, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func TestSessionActivityRecordsPlanAndPermission(t *testing.T) {
	store, sess := newActivityTestStore(t)
	activity := newSessionActivity(store, sess.ID, "turn-1")
	activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"plan","entries":[` +
		`{"content":"Read the run","status":"completed","priority":"high"},` +
		`{"content":"Check allocations","status":"pending","priority":"medium"}]}}`))
	activity.permission("t1", "selected", "opt-allow", "allow_always")
	activity.close(context.Background())

	events := activityEvents(t, store, sess.ID)
	if len(events) != 2 {
		t.Fatalf("want a plan and a permission, got %d", len(events))
	}
	entries, _ := activityPayload(t, events[0])["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("plan lost its steps: %v", entries)
	}
	first, _ := entries[0].(map[string]any)
	if first["content"] != "Read the run" || first["status"] != "completed" {
		t.Fatalf("plan step is wrong: %v", first)
	}
	permission := activityPayload(t, events[1])
	if permission["outcome"] != "selected" || permission["option_kind"] != "allow_always" {
		t.Fatalf("permission lost what was decided: %v", permission)
	}
}

// An oversized tool input is dropped whole rather than cut: half a JSON object
// is not evidence, and it is the one field an agent can make arbitrarily large.
func TestSessionActivityDropsOversizedToolInput(t *testing.T) {
	store, sess := newActivityTestStore(t)
	activity := newSessionActivity(store, sess.ID, "turn-1")
	huge := strings.Repeat("x", sessionActivityInputBytes+1)
	activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call","toolCallId":"t1",` +
		`"title":"Write","kind":"edit","rawInput":{"body":"` + huge + `"}}}`))
	activity.close(context.Background())

	events := activityEvents(t, store, sess.ID)
	if len(events) != 1 {
		t.Fatalf("want one start, got %d", len(events))
	}
	payload := activityPayload(t, events[0])
	if payload["input"] != nil {
		t.Fatalf("oversized input was kept: %v", payload["input"])
	}
	if payload["title"] != "Write" {
		t.Fatalf("dropping the input must not drop the call: %v", payload)
	}
}

// A turn that outruns its narration budget stops narrating and says so. A
// silently short list reads as a complete account of a turn it only partly saw.
func TestSessionActivityReportsWhatItCouldNotNarrate(t *testing.T) {
	store, sess := newActivityTestStore(t)
	activity := newSessionActivity(store, sess.ID, "turn-1")
	for index := range sessionActivityMaxEvents + 20 {
		id := "t" + strconv.Itoa(index)
		activity.observe(json.RawMessage(
			`{"update":{"sessionUpdate":"tool_call","toolCallId":"` + id + `","title":"Read","kind":"read"}}`))
	}
	activity.close(context.Background())

	events := activityEvents(t, store, sess.ID)
	if len(events) != sessionActivityMaxEvents+1 {
		t.Fatalf("want the budget plus one elision marker, got %d", len(events))
	}
	last := events[len(events)-1]
	if last.Type != session.EventActivityElided {
		t.Fatalf("the budget was spent without saying so, last event is %s", last.Type)
	}
	if dropped, _ := activityPayload(t, last)["dropped"].(float64); int(dropped) != 20 {
		t.Fatalf("wrong drop count: %v", dropped)
	}
}

// activityTestClock is a hand-advanced clock. The alive window is a minute long
// and the events under test are counted in windows, so the test moves time
// itself rather than waiting for it or shrinking the constant it is asserting.
type activityTestClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *activityTestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *activityTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// A turn whose provider CLI is retrying 429s inside itself streams frames and
// narrates nothing: no tool calls, no plan, no thoughts. Before this it produced
// no events at all, and a client watching for silence could only read it as
// dead — Responder widened its silent-turn deadline from 15m to 45m because
// cancelling one replayed the work into a fresh session that inherited the same
// throttle (2026-08-15). The pulse is per window, not per frame, and its
// counters are cumulative so a redelivered event is not read as new progress.
func TestAStreamingTurnWithNothingToNarrateStillReportsItIsAlive(t *testing.T) {
	store, sess := newActivityTestStore(t)
	clock := &activityTestClock{at: time.Unix(1_760_000_000, 0)}
	activity := newSessionActivity(store, sess.ID, "turn-1", clock.now)

	// Two full windows of frames, forty frames apiece, with nothing narratable
	// in any of them.
	for window := range 2 {
		for range 40 {
			activity.frame(512)
		}
		if window == 0 {
			clock.advance(sessionActivityAliveInterval)
		}
	}
	activity.close(context.Background())

	events := activityEvents(t, store, sess.ID)
	if len(events) != 1 {
		t.Fatalf("want one pulse for the one elapsed window, got %d: %v", len(events), eventTypes(events))
	}
	if events[0].Type != session.EventProviderAlive || events[0].Version != 1 {
		t.Fatalf("pulse = %s v%d", events[0].Type, events[0].Version)
	}
	payload := activityPayload(t, events[0])
	if frames, _ := payload["frames"].(float64); int(frames) != 41 {
		t.Fatalf("pulse counted %v frames, want every frame of the turn so far", payload["frames"])
	}
	if bytes, _ := payload["bytes"].(float64); int(bytes) != 41*512 {
		t.Fatalf("pulse counted %v bytes", payload["bytes"])
	}

	// A second window with frames still flowing pulses again, and the counters
	// have moved — that is how a client tells progress from a redelivery.
	clock.advance(sessionActivityAliveInterval)
	activity = newSessionActivity(store, sess.ID, "turn-1", clock.now)
	clock.advance(sessionActivityAliveInterval)
	activity.frame(512)
	clock.advance(sessionActivityAliveInterval)
	activity.frame(512)
	activity.close(context.Background())

	events = activityEvents(t, store, sess.ID)
	if len(events) != 3 {
		t.Fatalf("want a pulse per elapsed window, got %d: %v", len(events), eventTypes(events))
	}
	first, second := activityPayload(t, events[1]), activityPayload(t, events[2])
	if first["frames"].(float64) >= second["frames"].(float64) {
		t.Fatalf("pulse counters did not advance: %v then %v", first, second)
	}
}

// The pulse is for turns that have gone QUIET. A turn narrating its work is not
// quiet, and a second line saying so would double-narrate every long tool call —
// teaching a client to discount the one signal this exists to send.
func TestAliveIsSilentWhileTheTurnIsNarratingItsWork(t *testing.T) {
	store, sess := newActivityTestStore(t)
	clock := &activityTestClock{at: time.Unix(1_760_000_000, 0)}
	activity := newSessionActivity(store, sess.ID, "turn-1", clock.now)

	// A window passes, but the frame that ends it carried a tool call, which the
	// frame loop narrates before reporting the frame.
	clock.advance(sessionActivityAliveInterval)
	activity.observe(json.RawMessage(
		`{"update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Read job","kind":"read"}}`))
	activity.frame(512)
	// And the window after it is spent inside that same tool call.
	clock.advance(sessionActivityAliveInterval - time.Second)
	activity.frame(512)
	activity.close(context.Background())

	events := activityEvents(t, store, sess.ID)
	if len(events) != 1 || events[0].Type != session.EventToolStarted {
		t.Fatalf("want only the tool start, got %v", eventTypes(events))
	}
}

// Nothing about narration may fail a turn, and nothing about it may block the
// frame loop. A recorder with no store is the degenerate case of both.
func TestSessionActivityWithoutStoreIsInert(t *testing.T) {
	var activity *sessionActivity
	activity.observe(json.RawMessage(`{"update":{"sessionUpdate":"tool_call","toolCallId":"t1"}}`))
	activity.permission("t1", "selected", "opt", "allow_once")
	activity.close(context.Background())
	if newSessionActivity(nil, "sess", "turn") != nil {
		t.Fatal("a recorder without a store must not exist")
	}
}
