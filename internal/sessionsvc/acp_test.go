package sessionsvc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/session"
)

func TestSessionTurnRunnerNewThenExactLoadAndPrivateProjection(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	first := fixture.submit(t, "first prompt")
	firstResult, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, first)
	if err != nil {
		t.Fatal(err)
	}
	if firstResult.AssistantMessage != "hello world" {
		t.Fatalf("first assistant message = %q", firstResult.AssistantMessage)
	}
	if firstResult.SendState != session.SendStateSent {
		t.Fatalf("first send state = %q", firstResult.SendState)
	}

	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.NativeSessionID != "native-1" {
		t.Fatalf("native session = %q, want native-1", bound.NativeSessionID)
	}
	if _, err := os.Stat(filepath.Join(fixture.private, "codex", "profiles", "work", "native-history")); err != nil {
		t.Fatalf("native session state was not retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.private, "codex", "profiles", "work", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("projected credential remains after success: %v", err)
	}
	for _, name := range []string{"env", "mcp.json", "INSTRUCTIONS.md"} {
		if _, err := os.Stat(filepath.Join(fixture.private, name)); !os.IsNotExist(err) {
			t.Fatalf("projected %s remains after success: %v", name, err)
		}
	}
	assertMode(t, fixture.private, 0o700)

	fixture.session = bound
	second := fixture.submit(t, "second prompt")
	secondResult, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, second)
	if err != nil {
		t.Fatal(err)
	}
	if secondResult.AssistantMessage != "hello world" {
		t.Fatalf("load history leaked into assistant message: %q", secondResult.AssistantMessage)
	}
	methods := readSessionACPLog(t, fixture.childLog)
	want := []string{"initialize", "session/new", "session/prompt", "initialize", "session/load", "session/prompt"}
	if fmt.Sprint(methods) != fmt.Sprint(want) {
		t.Fatalf("ACP methods = %v, want %v", methods, want)
	}
	if got := readFile(t, fixture.envLog); !strings.Contains(got, "config="+fixture.private) ||
		!strings.Contains(got, "repo="+fixture.repo) || !strings.Contains(got, "run=session-") ||
		!strings.Contains(got, "files=env,mcp.json,INSTRUCTIONS.md") ||
		strings.Contains(got, "secret") || !strings.Contains(got, "mcp= openai=") {
		t.Fatalf("child environment was not private: %q", got)
	}
	if got := readFile(t, fixture.childLog); !strings.Contains(got, `"cwd":"`+fixture.session.Workspace+`"`) {
		t.Fatalf("ACP cwd was not the immutable fork workspace: %q", got)
	}
	if got := readFile(t, fixture.runtimeLog); !strings.Contains(got, "coop.run") ||
		!strings.Contains(got, "session-") ||
		!strings.Contains(got, "com.docker.compose.project=") ||
		!strings.Contains(got, "com.docker.compose.project.working_dir=") {
		t.Fatalf("runtime cleanup label was not recorded: %q", got)
	}
}

func TestInvalidStructuredResultIsRepairedBeforeTheTurnCompletes(t *testing.T) {
	// A production conversation emitted one extra closing brace three times. Coop
	// marked every attempt complete, so Responder spent its correction budget and
	// showed a generic failure even though the intended reply was recoverable.
	fixture := newSessionACPFixture(t, "invalid-contract-once")
	leased := fixture.submitContract(t, "return the result")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, leased)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != session.TurnCompleted || result.AssistantMessage != `{"reply":"valid"}` {
		t.Fatalf("completed contracted turn = %+v", result)
	}
	if result.Usage.InputTokens != 20 || result.Usage.OutputTokens != 4 {
		t.Fatalf("repair usage = %+v, want both provider attempts", result.Usage)
	}
	methods := readSessionACPLog(t, fixture.childLog)
	if got := countStrings(methods, "session/prompt"); got != 2 {
		t.Fatalf("session/prompt calls = %d, want initial plus one repair; methods=%v", got, methods)
	}
	wire := readFile(t, fixture.childLog)
	if !strings.Contains(wire, "jv --assert-format --output detailed") ||
		!strings.Contains(wire, leased.OutputContract.SHA256) {
		t.Fatalf("initial prompt did not require exact self-validation: %s", wire)
	}
}

func TestRepeatedInvalidStructuredResultNeverCompletes(t *testing.T) {
	fixture := newSessionACPFixture(t, "invalid-contract-always")
	leased := fixture.submitContract(t, "return the result")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, leased)
	if err == nil {
		t.Fatal("repeated invalid structured output completed")
	}
	if result.State != session.TurnFailed || result.ErrorCode != session.CodeOutputContractFailed || result.AssistantMessage != "" {
		t.Fatalf("failed contracted turn = %+v, err=%v", result, err)
	}
	methods := readSessionACPLog(t, fixture.childLog)
	if got := countStrings(methods, "session/prompt"); got != 3 {
		t.Fatalf("session/prompt calls = %d, want three bounded attempts; methods=%v", got, methods)
	}
}

func TestSchemaValidSemanticResultWaitsForCallerAcceptance(t *testing.T) {
	fixture := newSessionACPFixture(t, "valid-contract")
	leased := fixture.submitSemanticContract(t, "return the result")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, leased)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != session.TurnAwaitingValidation || result.AssistantMessage != "" ||
		result.Candidate == nil || result.Candidate.Message != `{"reply":"valid"}` {
		t.Fatalf("semantic candidate = %+v", result)
	}
	accepted, err := fixture.store.CompleteTurn(context.Background(), session.CompleteTurnRequest{
		SessionID: fixture.session.ID, TurnID: leased.ID,
		CandidateSHA256: result.Candidate.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != session.TurnCompleted || accepted.AssistantMessage != `{"reply":"valid"}` ||
		accepted.ValidationReceipt == "" {
		t.Fatalf("accepted semantic result = %+v", accepted)
	}
}

// A production turn used its first shared attempt repairing malformed JSON,
// leaving only two chances to satisfy caller-owned semantic rules. Schema
// repair and semantic repair are different failure classes and must not spend
// the same budget.
func TestSchemaRepairDoesNotSpendASemanticCandidateAttempt(t *testing.T) {
	fixture := newSessionACPFixture(t, "invalid-contract-once")
	leased := fixture.submitSemanticContract(t, "return the result")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, leased)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != session.TurnAwaitingValidation || result.Candidate == nil {
		t.Fatalf("semantic candidate = %+v", result)
	}
	if result.Candidate.Attempt != 1 || result.ValidationAttempt != 1 {
		t.Fatalf("semantic attempt = candidate %d, turn %d; want 1 after schema repair",
			result.Candidate.Attempt, result.ValidationAttempt)
	}
	if got := countStrings(readSessionACPLog(t, fixture.childLog), "session/prompt"); got != 2 {
		t.Fatalf("prompt calls = %d, want malformed JSON plus its schema repair", got)
	}
}

func TestEveryStructuredCandidateIsToldToSelfValidateBeforeReturning(t *testing.T) {
	contract := &session.OutputContract{SHA256: strings.Repeat("a", 64), JSONSchema: json.RawMessage(`{"type":"object"}`)}
	for name, prompt := range map[string]string{
		"initial": sessionOutputContractInitialPrompt("answer", contract),
		"repair":  sessionOutputContractRepairPrompt(contract, 2, errors.New("missing required field")),
	} {
		if !strings.Contains(prompt, "jv --assert-format") ||
			!strings.Contains(prompt, "only after jv exits successfully") ||
			!strings.Contains(prompt, `<json-schema>{"type":"object"}</json-schema>`) {
			t.Fatalf("%s structured prompt does not require model-side validation:\n%s", name, prompt)
		}
	}
}

func TestRejectedSemanticResultRepromptsTheSameNativeTurn(t *testing.T) {
	fixture := newSessionACPFixture(t, "valid-contract")
	leased := fixture.submitSemanticContract(t, "return the result")
	first, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, leased)
	if err != nil {
		t.Fatal(err)
	}
	if first.Candidate == nil || first.Candidate.Attempt != 1 {
		t.Fatalf("first candidate = %+v", first)
	}
	if _, err := fixture.store.RejectTurnCandidate(context.Background(), session.RejectTurnCandidateRequest{
		SessionID: fixture.session.ID, TurnID: leased.ID,
		CandidateSHA256: first.Candidate.SHA256,
		Violations:      []string{"completion.status contradicts current evidence"},
	}); err != nil {
		t.Fatal(err)
	}
	leasedAgain, ok, err := fixture.store.LeaseNextTurn(context.Background(), fixture.session.ID)
	if err != nil || !ok || leasedAgain.ID != leased.ID || leasedAgain.ValidationAttempt != 1 {
		t.Fatalf("semantic repair lease = %+v, ok=%v, err=%v", leasedAgain, ok, err)
	}
	fixture.session, err = fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, leasedAgain)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != leased.ID || second.Candidate == nil || second.Candidate.Attempt != 2 {
		t.Fatalf("second candidate = %+v", second)
	}
	methods := readSessionACPLog(t, fixture.childLog)
	if got := countStrings(methods, "session/prompt"); got != 2 || countStrings(methods, "session/load") != 1 {
		t.Fatalf("semantic repair did not resume the native session: methods=%v", methods)
	}
}

func countStrings(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

// One accepted production result was discarded after the recovery burst kept Coop's single
// SQLite connection busy for just over two seconds. The model had finished, so local receipt
// contention must wait longer than best-effort process cleanup instead of failing the turn.
func TestCompletedModelResultSurvivesShortStoreContention(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	leased := fixture.submit(t, "completed answer")
	if _, err := fixture.store.MarkTurnSendIntent(context.Background(), fixture.session.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.MarkTurnSent(context.Background(), fixture.session.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	fixture.runner.completion = delayedCompleteTurnStore{turnCompletionStore: fixture.store, delay: 2500 * time.Millisecond}

	completed, completeErr := fixture.runner.completeTurn(fixture.session, leased, "durable answer", nil, session.Usage{})
	if completeErr != nil {
		t.Fatalf("completed result was lost behind short store contention: %v", completeErr)
	}
	if completed.State != session.TurnCompleted || completed.AssistantMessage != "durable answer" {
		t.Fatalf("completed turn = %+v", completed)
	}
}

type delayedCompleteTurnStore struct {
	turnCompletionStore
	delay time.Duration
}

func (s delayedCompleteTurnStore) CompleteTurn(ctx context.Context, req session.CompleteTurnRequest) (session.Turn, error) {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return session.Turn{}, ctx.Err()
	case <-timer.C:
		return s.turnCompletionStore.CompleteTurn(ctx, req)
	}
}

// An ACP session's MCP servers arrive in the session/new parameter or not at all: the adapter
// takes no flags, so the mounted mcp.json that --mcp-config points the claude CLI at is invisible
// to it. Production ran that way — 706 tool calls from claude sessions, not one of them mcp.*.
func TestAClaudeACPSessionIsHandedTheSharedMCPServers(t *testing.T) {
	// The token must come from the session's own projected env file, not from whatever the
	// controller process happens to be holding.
	t.Setenv("EMISAR_TOKEN", "ambient-token")
	fixture := newSessionACPFixture(t, "normal", "claude@work")
	first := fixture.submit(t, "first prompt")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, first); err != nil {
		t.Fatal(err)
	}
	want := `[{"headers":[{"name":"Authorization","value":"Bearer observe-only"}],` +
		`"name":"emisar","type":"http","url":"https://example.invalid/mcp"}]`
	if got := sessionACPRequestMCPServers(t, fixture.childLog, "session/new"); got != want {
		t.Fatalf("session/new mcpServers = %s, want %s", got, want)
	}

	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.session = bound
	second := fixture.submit(t, "second prompt")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, second); err != nil {
		t.Fatal(err)
	}
	// The adapter fingerprints cwd plus this list to decide whether a load can reuse its process,
	// so a load carrying a different list quietly restarts the conversation it was resuming.
	if got := sessionACPRequestMCPServers(t, fixture.childLog, "session/load"); got != want {
		t.Fatalf("session/load mcpServers = %s, want the session/new list %s", got, want)
	}
}

// Codex reads the same servers from the generated [mcp_servers.*] file its box mounts. Sending
// them here as well would register every server twice in one session.
func TestAnAgentWithAGeneratedMCPConfigIsHandedNoACPServers(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	leased := fixture.submit(t, "first prompt")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, leased); err != nil {
		t.Fatal(err)
	}
	if got := sessionACPRequestMCPServers(t, fixture.childLog, "session/new"); got != "[]" {
		t.Fatalf("codex session/new mcpServers = %s, want an empty list", got)
	}
}

// sessionACPRequestMCPServers returns the "mcpServers" parameter of the first logged request for
// method, exactly as it went over the wire.
func sessionACPRequestMCPServers(t *testing.T, path, method string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(readFile(t, path)), "\n") {
		var frame struct {
			Method string `json:"method"`
			Params struct {
				MCPServers json.RawMessage `json:"mcpServers"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &frame) == nil && frame.Method == method {
			return string(frame.Params.MCPServers)
		}
	}
	t.Fatalf("no %s request was sent", method)
	return ""
}

func TestSessionACPRejectionDetailIsUsefulAndBounded(t *testing.T) {
	for name, expect := range map[string]struct {
		raw  string
		want string
	}{
		"carries the reason": {
			`{"code":-32000,"message":"You have hit your usage limit. Try again Aug 11."}`,
			"ACP request was rejected: You have hit your usage limit. Try again Aug 11.",
		},
		// An adapter is not a trusted formatter: a turn detail is one line.
		"collapses whitespace": {
			`{"code":-32000,"message":"first line\n\tsecond line"}`,
			"ACP request was rejected: first line second line",
		},
		"no message":    {`{"code":-32000}`, "ACP request was rejected"},
		"not an object": {`"boom"`, "ACP request was rejected"},
		"empty message": {`{"message":"   "}`, "ACP request was rejected"},
	} {
		if got := sessionACPRejectionDetail(json.RawMessage(expect.raw)); got != expect.want {
			t.Fatalf("%s = %q, want %q", name, got, expect.want)
		}
	}
	long := `{"message":"` + strings.Repeat("x", 5000) + `"}`
	got := sessionACPRejectionDetail(json.RawMessage(long))
	if len(got) > sessionACPRejectionLimit+64 || !strings.HasSuffix(got, "…") {
		t.Fatalf("oversized rejection was not bounded: %d bytes", len(got))
	}
}

// A live Responder turn switched from codex@emisar to codex@oncall after the
// first credential hit its limit, then tried to load emisar's native session
// through oncall's credential store. Codex rejected it and the queue stayed
// blocked even though oncall had 91% of its allowance left.
func TestAProviderCredentialRotationStartsANewNativeSession(t *testing.T) {
	fixture := newSessionACPFixture(t, "rate-limited-once")
	fixture.signIn(t, "codex", "backup")
	t.Setenv("COOP_TEST_SESSION_LIMIT_MARKER", filepath.Join(t.TempDir(), "limited"))
	leased := fixture.submit(t, "investigate")
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup")
	result, err := fixture.runner.Run(ctx, fixture.session, leased)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != session.TurnCompleted || result.AssistantMessage != "rotated answer" {
		t.Fatalf("rotated turn = %+v", result)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Target != "codex@backup" {
		t.Fatalf("session target = %q, want codex@backup", bound.Target)
	}
	// The new credential owns a different native-session store. Its child must
	// start a session instead of trying to load the first account's binding.
	if bound.NativeSessionID != "native-1" {
		t.Fatalf("native session = %q, want the new account's native-1", bound.NativeSessionID)
	}
	methods := readSessionACPLog(t, fixture.childLog)
	want := []string{"initialize", "session/new", "session/prompt", "initialize", "session/new", "session/prompt"}
	if fmt.Sprint(methods) != fmt.Sprint(want) {
		t.Fatalf("ACP methods = %v, want %v", methods, want)
	}
	events, err := fixture.store.ListEvents(context.Background(), fixture.session.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	rotated := ""
	backoff := ""
	for _, event := range events {
		if event.Type == session.EventSessionTargetRotated {
			rotated = string(event.Payload)
		}
		if event.Type == session.EventProviderBackoff {
			backoff = string(event.Payload)
		}
	}
	if rotated != `{"from":"codex@work","native_session_reset":true,"to":"codex@backup"}` {
		t.Fatalf("rotation event = %s", rotated)
	}
	// The limit is audible before the rotation: a client watching events can
	// tell a throttled rung from a dead transport (2026-08-15, when it could
	// not, Responder cancelled crawling turns on that ambiguity).
	if !strings.Contains(backoff, `"target":"codex@work"`) ||
		!strings.Contains(backoff, `"next_target":"codex@backup"`) {
		t.Fatalf("backoff event = %s", backoff)
	}
}

// The all-cooling branch narrates too, so the reset the provider promised is
// on the stream a client can read, not only inside the error detail prose.
func TestAnExhaustedLadderNarratesItsBackoffBeforeFailing(t *testing.T) {
	fixture := newSessionACPFixture(t, "rate-limited")
	fixture.signIn(t, "codex", "backup")
	leased := fixture.submit(t, "investigate")
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup")
	_, err := fixture.runner.Run(ctx, fixture.session, leased)
	if err == nil {
		t.Fatal("an exhausted ladder completed the turn")
	}
	events, err := fixture.store.ListEvents(context.Background(), fixture.session.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	backoffs := 0
	allLimited := ""
	for _, event := range events {
		if event.Type == session.EventProviderBackoff {
			backoffs++
			allLimited = string(event.Payload)
		}
	}
	if backoffs == 0 {
		t.Fatal("an exhausted ladder failed silently; no provider.backoff event landed")
	}
	if !strings.Contains(allLimited, `"all_limited_until"`) {
		t.Fatalf("the final backoff event carries no reset: %s", allLimited)
	}
}

// A crawling turn has to say WHICH wait it is on and how long it is. During the
// 2026-08-15 storm a throttled turn produced no events at all, so Responder's
// silent-turn deadline cancel-replayed healthy crawling turns into fresh
// sessions that inherited the same throttle; the deadline was widened 15m→45m
// as a stopgap. Restoring it needs more than "something happened": a client has
// to count the backoffs (so a redelivered event is not read as new progress)
// and read the wait off the provider's own reset — an hour here — rather than
// guess from a bounded-backoff placeholder.
func TestEachBackoffOnATurnIsNumberedAndCarriesItsWait(t *testing.T) {
	fixture := newSessionACPFixture(t, "rate-limited")
	fixture.signIn(t, "codex", "backup")
	leased := fixture.submit(t, "investigate")
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup")
	if _, err := fixture.runner.Run(ctx, fixture.session, leased); err == nil {
		t.Fatal("an exhausted ladder completed the turn")
	}
	backoffs := sessionBackoffPayloads(t, fixture)
	if len(backoffs) != 2 {
		t.Fatalf("a two-rung ladder narrated %d backoffs, want one per rung: %v", len(backoffs), backoffs)
	}
	for i, payload := range backoffs {
		attempt, ok := payload["attempt"].(float64)
		if !ok || int(attempt) != i+1 {
			t.Fatalf("backoff %d is numbered %v, want %d", i, payload["attempt"], i+1)
		}
		wait, ok := payload["retry_after_seconds"].(float64)
		if !ok || wait < 3000 || wait > 3700 {
			t.Fatalf("backoff %d waits %v, want the provider's hour-long reset", i, payload["retry_after_seconds"])
		}
	}
}

// The heartbeat is a signal, so it must be silent on a turn nobody throttled —
// including the turn whose own answer quotes limit wording, which the ladder
// deliberately refuses to treat as evidence. A backoff event on a healthy turn
// would teach a client to ignore the one case it exists for.
func TestATurnThatIsNotThrottledNarratesNoBackoff(t *testing.T) {
	for name, scenario := range map[string]string{
		"a healthy turn":              "normal",
		"limit wording in the answer": "limit-prose",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSessionACPFixture(t, scenario)
			fixture.signIn(t, "codex", "backup")
			leased := fixture.submit(t, "investigate")
			ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup")
			if _, err := fixture.runner.Run(ctx, fixture.session, leased); err != nil {
				t.Fatal(err)
			}
			if backoffs := sessionBackoffPayloads(t, fixture); len(backoffs) != 0 {
				t.Fatalf("an unthrottled turn narrated %d backoffs: %v", len(backoffs), backoffs)
			}
		})
	}
}

// A turn crawling inside the provider CLI's own retry loop is the shape the
// 2026-08-15 storm mostly took: frames keep arriving, and not one of them is a
// tool call, a plan, or a thought. Coop's ladder never sees a limit, so it
// narrates no backoff, and before the pulse the whole turn was silent — which is
// exactly what a dead one looks like, and why Responder's silent-turn deadline
// sits at 45m instead of 15m.
func TestATurnCrawlingInsideTheProviderIsAudibleWithoutAnyToolCalls(t *testing.T) {
	fixture := newSessionACPFixture(t, "slow-stream")
	// Half the alive window per clock read, so the streamed frames themselves
	// carry the turn across window boundaries deterministically.
	fixture.runner.activityClock = steppedClock(sessionActivityAliveInterval / 2)
	leased := fixture.submit(t, "crawl")
	ctx := contextWithTurnDeadline(t)
	if _, err := fixture.runner.Run(ctx, fixture.session, leased); err != nil {
		t.Fatal(err)
	}
	pulses := sessionAlivePayloads(t, fixture)
	if len(pulses) < 2 {
		t.Fatalf("a crawling turn produced %d pulses; a client watching it cannot tell it from a dead one", len(pulses))
	}
	// Throttled, not a per-frame ticker: the child streams sessionSlowStreamFrames
	// chunks plus its response, and the pulse must cost fewer events than that.
	if len(pulses) >= sessionSlowStreamFrames {
		t.Fatalf("the pulse fired once per frame (%d pulses); it must be bounded by its window", len(pulses))
	}
	previous := 0.0
	for i, pulse := range pulses {
		frames, ok := pulse["frames"].(float64)
		if !ok || frames <= previous {
			t.Fatalf("pulse %d counters did not advance: %v after %v", i, pulse, previous)
		}
		if bytes, _ := pulse["bytes"].(float64); bytes <= 0 {
			t.Fatalf("pulse %d counted no bytes: %v", i, pulse)
		}
		previous = frames
	}
	// A pulse is not narration: nothing here claims the model did anything.
	events, err := fixture.store.ListEvents(context.Background(), fixture.session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == session.EventToolStarted || event.Type == session.EventProviderBackoff {
			t.Fatalf("a crawling turn narrated %s, which nothing in it did", event.Type)
		}
	}
}

// An ordinary turn already says what it is doing, so it owes no pulse. One here
// would be a second line about the same second, and a client that learns the
// event is routine stops reading it as the signal it is.
func TestAnOrdinaryTurnNarratesNoPulse(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	leased := fixture.submit(t, "ordinary work")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, leased); err != nil {
		t.Fatal(err)
	}
	if pulses := sessionAlivePayloads(t, fixture); len(pulses) != 0 {
		t.Fatalf("an ordinary turn narrated %d pulses: %v", len(pulses), pulses)
	}
}

// steppedClock advances one step on every read. The alive window is a minute
// long and the frames that cross it come from a real child process, so a wall
// clock would make this test either minutes long or dependent on how loaded the
// machine is; stepping per read ties the window to the frames themselves.
func steppedClock(step time.Duration) func() time.Time {
	start := time.Now()
	var reads atomic.Int64
	return func() time.Time { return start.Add(time.Duration(reads.Add(1)) * step) }
}

// sessionAlivePayloads decodes the turn's provider.alive pulses in sequence
// order — the same bytes a client polling GET /v1/sessions/{id}/events reads.
func sessionAlivePayloads(t *testing.T, fixture *sessionACPFixture) []map[string]any {
	t.Helper()
	events, err := fixture.store.ListEvents(context.Background(), fixture.session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var payloads []map[string]any
	for _, event := range events {
		if event.Type != session.EventProviderAlive {
			continue
		}
		if event.Version != 1 {
			t.Fatalf("provider.alive version = %d, want 1", event.Version)
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("alive payload %s: %v", event.Payload, err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

// sessionBackoffPayloads decodes the turn's provider.backoff narration in
// sequence order — the same bytes a client polling GET /v1/sessions/{id}/events
// reads off the wire.
func sessionBackoffPayloads(t *testing.T, fixture *sessionACPFixture) []map[string]any {
	t.Helper()
	events, err := fixture.store.ListEvents(context.Background(), fixture.session.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var payloads []map[string]any
	for _, event := range events {
		if event.Type != session.EventProviderBackoff {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("backoff payload %s: %v", event.Payload, err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

// Responder re-delivers a corrected turn on a higher rung: the rung that produced
// the answer being corrected is not the rung to correct it on. The floor has to
// apply to the FIRST delivery — a turn that spends an attempt on rung one and
// only then climbs has already spent the correction on the model being corrected.
func TestAnEscalatedTurnIsDeliveredFirstOnTheRungItNames(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	fixture.signIn(t, "codex", "backup")
	leased := fixture.submitFromRung(t, "re-deliver the correction", 1)
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup")
	result, err := fixture.runner.Run(ctx, fixture.session, leased)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != session.TurnCompleted {
		t.Fatalf("escalated turn = %+v", result)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Target != "codex@backup" {
		t.Fatalf("escalated turn ran on %q, want the rung it named", bound.Target)
	}
	// One prompt, not two: the floor is where the turn starts, not somewhere it
	// arrives after burning an attempt on the rung below.
	methods := readSessionACPLog(t, fixture.childLog)
	want := []string{"initialize", "session/new", "session/prompt"}
	if fmt.Sprint(methods) != fmt.Sprint(want) {
		t.Fatalf("ACP methods = %v, want %v", methods, want)
	}
	// Nothing was throttled, so nothing may claim it was.
	if backoffs := sessionBackoffPayloads(t, fixture); len(backoffs) != 0 {
		t.Fatalf("an escalated turn narrated %d backoffs: %v", len(backoffs), backoffs)
	}
}

// A turn with no floor is the old turn, byte for byte: it starts on the session's
// own rung and the ladder is untouched.
func TestATurnWithoutAnEscalationFloorStartsWhereTheSessionIs(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	fixture.signIn(t, "codex", "backup")
	leased := fixture.submit(t, "ordinary turn")
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup")
	if _, err := fixture.runner.Run(ctx, fixture.session, leased); err != nil {
		t.Fatal(err)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Target != "codex@work" {
		t.Fatalf("an unfloored turn moved the session to %q", bound.Target)
	}
	events, err := fixture.store.ListEvents(context.Background(), fixture.session.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == session.EventSessionTargetRotated {
			t.Fatalf("an unfloored turn rotated the session: %s", event.Payload)
		}
	}
}

// Two live Responder investigations stayed queued after their escalated
// Claude turns hit the weekly quota even though Codex was healthy. Omitting a
// floor did not help because the session's durable target was still Claude.
// A one-turn rewind must move the session to rung zero before any provider sees
// the prompt, and a cross-provider move must discard the foreign transcript.
func TestARewoundTurnStartsOnTheHealthyFirstRung(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal", "claude@work")
	fixture.signIn(t, "codex", "work")
	if _, err := fixture.store.BindNativeSession(
		context.Background(), fixture.session.ID, "native-claude",
	); err != nil {
		t.Fatal(err)
	}
	leased := fixture.submitRequest(t, session.SubmitTurnRequest{
		Prompt: "continue on the healthy fallback", RewindTarget: true,
	})
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "claude@work")
	result, err := fixture.runner.Run(ctx, fixture.session, leased)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != session.TurnCompleted {
		t.Fatalf("rewound turn = %+v", result)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Target != "codex@work" {
		t.Fatalf("rewound turn ran on %q, want codex@work", bound.Target)
	}
	events, err := fixture.store.ListEvents(context.Background(), fixture.session.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == session.EventSessionTargetRotated &&
			strings.Contains(string(event.Payload), `"from":"claude@work"`) &&
			strings.Contains(string(event.Payload), `"to":"codex@work"`) &&
			strings.Contains(string(event.Payload), `"native_session_reset":true`) {
			return
		}
	}
	t.Fatal("rewind did not publish a cross-provider target rotation")
}

// The floor composes with the ordinary ladder: it says where the turn starts, and
// a rate limit there still rotates UPWARD to the next free rung the way any other
// turn's would.
func TestAnEscalatedTurnStillRotatesUpwardWhenItsRungIsLimited(t *testing.T) {
	fixture := newSessionACPFixture(t, "rate-limited-once")
	fixture.signIn(t, "codex", "backup")
	fixture.signIn(t, "codex", "reserve")
	t.Setenv("COOP_TEST_SESSION_LIMIT_MARKER", filepath.Join(t.TempDir(), "limited"))
	leased := fixture.submitFromRung(t, "escalate then rotate", 1)
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup", "codex@reserve")
	result, err := fixture.runner.Run(ctx, fixture.session, leased)
	if err != nil {
		t.Fatal(err)
	}
	if result.AssistantMessage != "rotated answer" {
		t.Fatalf("escalated-then-rotated turn = %+v", result)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Target != "codex@reserve" {
		t.Fatalf("session target = %q, want the rung above the floor", bound.Target)
	}
	backoffs := sessionBackoffPayloads(t, fixture)
	if len(backoffs) != 1 {
		t.Fatalf("want one backoff for the limited floor rung, got %v", backoffs)
	}
	if backoffs[0]["target"] != "codex@backup" || backoffs[0]["next_target"] != "codex@reserve" {
		t.Fatalf("backoff did not rotate above the floor: %v", backoffs[0])
	}
}

// The floor is a floor, not a hint. When every rung at or above it is cooling the
// turn FAILS on the code a client retries against, because answering from the rung
// the caller escalated past is the outcome the parameter exists to prevent — and
// the failure has to name the floor, or an operator reads it as a dead ladder.
func TestAnEscalatedTurnNeverFallsBackBelowItsFloor(t *testing.T) {
	fixture := newSessionACPFixture(t, "rate-limited")
	fixture.signIn(t, "codex", "backup")
	leased := fixture.submitFromRung(t, "escalate into a storm", 1)
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup")
	result, err := fixture.runner.Run(ctx, fixture.session, leased)
	if err == nil {
		t.Fatal("a floored turn with no free rung above the floor completed")
	}
	if result.ErrorCode != sessionACPRateLimited ||
		!strings.Contains(result.ErrorDetail, "rung 1") {
		t.Fatalf("floored exhaustion = %+v, want a rate_limited failure naming the floor", result)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Target != "codex@backup" {
		t.Fatalf("session fell back to %q, below the floor it was given", bound.Target)
	}
}

// "No LOWER than" — a session already above the floor stays where it is. Reading
// the parameter as "start exactly here" would demote a session that had already
// climbed, which is the same bug in the other direction.
func TestAnEscalationFloorNeverDemotesASessionAlreadyAboveIt(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal", "codex@reserve")
	fixture.signIn(t, "codex", "work")
	fixture.signIn(t, "codex", "backup")
	leased := fixture.submitFromRung(t, "already high", 1)
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup", "codex@reserve")
	if _, err := fixture.runner.Run(ctx, fixture.session, leased); err != nil {
		t.Fatal(err)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Target != "codex@reserve" {
		t.Fatalf("a floor of rung 1 moved a session on rung 2 to %q", bound.Target)
	}
}

// A rung that is not on the ladder cannot be silently ignored: the turn would run
// on the rung the caller escalated past and read as an honored escalation. The
// service refuses the index at admission; this is the same refusal at the far end,
// where a policy edited between admission and lease lands.
func TestAnEscalationFloorOffTheLadderFailsTheTurnPlainly(t *testing.T) {
	for name, ladderRungs := range map[string][]string{
		"beyond the ladder": {"codex@work", "codex@backup"},
		"no ladder at all":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSessionACPFixture(t, "normal")
			fixture.signIn(t, "codex", "backup")
			leased := fixture.submitFromRung(t, "escalate off the end", 3)
			ctx := ladderContext(t, contextWithTurnDeadline(t), ladderRungs...)
			result, err := fixture.runner.Run(ctx, fixture.session, leased)
			if err == nil {
				t.Fatal("a turn floored above the ladder completed anyway")
			}
			if result.ErrorCode != sessionACPInvalidTarget ||
				!strings.Contains(result.ErrorDetail, "rung 3") {
				t.Fatalf("off-ladder floor = %+v, want an invalid_session_target naming the rung", result)
			}
		})
	}
}

func TestSessionTurnRunnerFailsTheTurnWhenEveryRungIsRateLimited(t *testing.T) {
	fixture := newSessionACPFixture(t, "rate-limited")
	fixture.signIn(t, "codex", "backup")
	leased := fixture.submit(t, "investigate")
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup")
	result, err := fixture.runner.Run(ctx, fixture.session, leased)
	if err == nil {
		t.Fatal("an exhausted ladder completed the turn")
	}
	// A queued turn does not wait out the reset — it fails on a code the client retries against.
	if result.ErrorCode != sessionACPRateLimited ||
		!strings.Contains(result.ErrorDetail, "every target in the policy ladder is rate limited until") {
		t.Fatalf("exhausted ladder turn = %+v", result)
	}
}

func TestSessionTurnRunnerRotatesOnlyOnAProvenRateLimit(t *testing.T) {
	for name, scenario := range map[string]string{
		// The rules card is explicit that a false positive must not hand work to another
		// provider: limit wording inside the model's own answer is not evidence of a limit.
		"assistant prose": "limit-prose",
		// A rejection that is not a limit surfaces, so a human fixes the real cause.
		"rejected request": "initialize-fail",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSessionACPFixture(t, scenario)
			fixture.signIn(t, "codex", "backup")
			leased := fixture.submit(t, "investigate")
			ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup")
			_, _ = fixture.runner.Run(ctx, fixture.session, leased)
			bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if bound.Target != "codex@work" {
				t.Fatalf("%s rotated the ladder to %q", name, bound.Target)
			}
		})
	}
}

func TestSessionTurnRunnerReusesWarmACPProcessAcrossTurns(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	warmContext := func() context.Context {
		return context.WithValue(contextWithTurnDeadline(t), sessionWarmIdleTimeoutContextKey{}, time.Minute)
	}
	first := fixture.submit(t, "first warm prompt")
	if _, err := fixture.runner.Run(warmContext(), fixture.session, first); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.private, "codex", "profiles", "work", "auth.json")); err != nil {
		t.Fatalf("warm credential projection was not retained: %v", err)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.session = bound
	second := fixture.submit(t, "second warm prompt")
	if _, err := fixture.runner.Run(warmContext(), fixture.session, second); err != nil {
		t.Fatal(err)
	}
	if got, want := readSessionACPLog(t, fixture.childLog), []string{
		"initialize", "session/new", "session/prompt", "session/prompt",
	}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("warm ACP methods = %v, want %v", got, want)
	}
	if got := strings.Count(strings.TrimSpace(readFile(t, fixture.envLog)), "\n") + 1; got != 1 {
		t.Fatalf("warm ACP child starts = %d, want 1", got)
	}
	if err := fixture.runner.CloseWarmSessions(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.private, "codex", "profiles", "work", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("warm credential remains after shutdown: %v", err)
	}
}

func TestSessionTurnRunnerPreparesAndExpiresWarmExecution(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	if err := fixture.runner.PrepareSession(contextWithTurnDeadline(t), fixture.session, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.NativeSessionID != "" {
		t.Fatalf("preparation consumed durable native identity: %q", bound.NativeSessionID)
	}
	if got, want := readSessionACPLog(t, fixture.childLog), []string{"initialize", "session/new"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("prepared ACP methods = %v, want %v", got, want)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(fixture.private, "codex", "profiles", "work", "auth.json")); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("warm execution did not expire and remove projected credentials")
}

func TestSessionTurnRunnerPreparedProcessHandlesFirstPromptWithoutRestart(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	t.Cleanup(func() { _ = fixture.runner.CloseWarmSessions() })
	if err := fixture.runner.PrepareSession(contextWithTurnDeadline(t), fixture.session, time.Minute); err != nil {
		t.Fatal(err)
	}
	turn := fixture.submit(t, "use the prepared process")
	ctx := context.WithValue(contextWithTurnDeadline(t), sessionWarmIdleTimeoutContextKey{}, time.Minute)
	if _, err := fixture.runner.Run(ctx, fixture.session, turn); err != nil {
		t.Fatal(err)
	}
	if got, want := readSessionACPLog(t, fixture.childLog), []string{
		"initialize", "session/new", "session/prompt",
	}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("prepared first-turn methods = %v, want %v", got, want)
	}
	if got := strings.Count(strings.TrimSpace(readFile(t, fixture.envLog)), "\n") + 1; got != 1 {
		t.Fatalf("prepared ACP child starts = %d, want 1", got)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.NativeSessionID != "native-1" {
		t.Fatalf("first prompt native session = %q, want native-1", bound.NativeSessionID)
	}
}

func TestSessionTurnRunnerFallsBackToColdWhenCredentialCannotCoverWarmLease(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	credential := filepath.Join(fixture.source, "codex", "profiles", "work", "auth.json")
	if err := os.WriteFile(credential, []byte(codexTestCredential(time.Now().Add(30*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	turn := fixture.submit(t, "do not trade correctness for warmth")
	ctx := context.WithValue(contextWithTurnDeadline(t), sessionWarmIdleTimeoutContextKey{}, time.Hour)
	if _, err := fixture.runner.Run(ctx, fixture.session, turn); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.WarmSessionReady(fixture.session) {
		t.Fatal("short-lived credentials unexpectedly left a warm execution")
	}
	if _, err := os.Stat(filepath.Join(fixture.private, "codex", "profiles", "work", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("cold fallback credential remains: %v", err)
	}
}

func TestSessionTurnRunnerSendsDurableImageArtifact(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	data := []byte("\x89PNG\r\n\x1a\nimage")
	digest := sha256.Sum256(data)
	turn := fixture.submitArtifacts(t, "inspect screenshot", []session.InputArtifact{{
		Name: "bug.png", MediaType: "image/png",
		SHA256: hex.EncodeToString(digest[:]), Data: data,
	}})
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn); err != nil {
		t.Fatal(err)
	}
	wire := readFile(t, fixture.childLog)
	if !strings.Contains(wire, `"type":"image"`) ||
		!strings.Contains(wire, `"mimeType":"image/png"`) ||
		!strings.Contains(wire, base64.StdEncoding.EncodeToString(data)) {
		t.Fatalf("ACP image prompt was not projected: %q", wire)
	}
}

func TestSessionTurnRunnerCapturesGeneratedImageOutsideTranscript(t *testing.T) {
	fixture := newSessionACPFixture(t, "image-output")
	turn := fixture.submit(t, "generate a chart")
	// This fixture moves more than 4 MiB through base64 JSON and SQLite. Keep it
	// bounded, but do not make package-parallel race-detector load a correctness
	// failure: the ordinary 5-second fixture deadline is for small ACP frames.
	completed, err := fixture.runner.Run(contextWithTurnTimeout(t, 15*time.Second), fixture.session, turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.OutputArtifacts) != 1 {
		t.Fatalf("output artifacts = %+v", completed.OutputArtifacts)
	}
	artifact := completed.OutputArtifacts[0]
	if artifact.MediaType != "image/png" || artifact.Bytes <= sessionACPTranscriptLimit || len(artifact.Data) != 0 {
		t.Fatalf("output artifact metadata = %+v", artifact)
	}
	got, err := fixture.store.GetOutputArtifact(context.Background(), fixture.session.ID, turn.ID, artifact.ID)
	if err != nil || int64(len(got.Data)) != artifact.Bytes {
		t.Fatalf("stored output artifact bytes=%d err=%v", len(got.Data), err)
	}
}

func TestSessionTurnRunnerCapturesGeneratedImageFromToolUpdate(t *testing.T) {
	fixture := newSessionACPFixture(t, "tool-image-output")
	turn := fixture.submit(t, "generate a chart with an image tool")
	completed, err := fixture.runner.Run(contextWithTurnTimeout(t, 15*time.Second), fixture.session, turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.OutputArtifacts) != 1 || completed.OutputArtifacts[0].Name != "generated-1.png" {
		t.Fatalf("output artifacts = %+v", completed.OutputArtifacts)
	}
	artifact, err := fixture.store.GetOutputArtifact(
		context.Background(), fixture.session.ID, turn.ID, completed.OutputArtifacts[0].ID,
	)
	if err != nil || len(artifact.Data) <= sessionACPTranscriptLimit {
		t.Fatalf("stored tool image bytes=%d err=%v", len(artifact.Data), err)
	}
}

func TestSessionTurnRunnerStreamsLargeToolOutputWithoutAbortingTurn(t *testing.T) {
	fixture := newSessionACPFixture(t, "large-tool-output")
	turn := fixture.submit(t, "investigate all relevant evidence")
	completed, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if err != nil {
		t.Fatal(err)
	}
	if completed.AssistantMessage != "investigation complete" {
		t.Fatalf("assistant message = %q", completed.AssistantMessage)
	}
}

func TestSessionTurnRunnerRejectsLoadFailureAndBindsConflicts(t *testing.T) {
	fixture := newSessionACPFixture(t, "load-fail")
	bound, err := fixture.store.BindNativeSession(context.Background(), fixture.session.ID, "native-existing")
	if err != nil {
		t.Fatal(err)
	}
	fixture.session = bound
	turn := fixture.submit(t, "load exactly this")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn); err == nil {
		t.Fatal("failed session/load unexpectedly succeeded")
	}
	got, err := fixture.store.GetTurn(context.Background(), fixture.session.ID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != session.TurnFailed || got.ErrorCode != sessionACPProtocolError {
		t.Fatalf("failed load turn = %+v", got)
	}
	// The adapter's own reason has to survive to the turn. Without it every
	// rejection reads the same, and an operator cannot tell a spent quota from a
	// retired model from a revoked login.
	if got.ErrorDetail != "ACP request was rejected: load failed" {
		t.Fatalf("rejection lost the adapter's reason: %q", got.ErrorDetail)
	}
	if _, err := fixture.store.BindNativeSession(context.Background(), fixture.session.ID, "another"); session.CodeOf(err) != session.CodeNativeSessionConflict {
		t.Fatalf("native binding conflict = %v", err)
	}
}

func TestSessionTurnRunnerPreservesInitializeFailure(t *testing.T) {
	fixture := newSessionACPFixture(t, "initialize-fail")
	turn := fixture.submit(t, "initialize failure")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn); err == nil {
		t.Fatal("initialize failure unexpectedly succeeded")
	} else if strings.Contains(err.Error(), "send-intent checkpoint") || !strings.Contains(err.Error(), "ACP request was rejected") {
		t.Fatalf("initialize failure was relabeled: %v", err)
	}
}

func TestSessionTurnRunnerReportsSafeChildLaunchDiagnostic(t *testing.T) {
	fixture := newSessionACPFixture(t, "missing-image")
	turn := fixture.submit(t, "launch failure")
	_, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if err == nil || !strings.Contains(err.Error(), "Coop box image is not built; run 'coop build'") {
		t.Fatalf("missing image failure = %v", err)
	}
	got, getErr := fixture.store.GetTurn(context.Background(), fixture.session.ID, turn.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.State != session.TurnFailed || got.ErrorCode != sessionACPProcessError ||
		!strings.Contains(got.ErrorDetail, "Coop box image is not built") {
		t.Fatalf("missing image turn = %+v", got)
	}
}

func TestSafeSessionACPExitDetailSuppressesArbitraryStderr(t *testing.T) {
	stderr := "provider failed with bearer secret-token and internal stack trace"
	if got := safeSessionACPExitDetail(stderr); got != "" {
		t.Fatalf("unsafe child diagnostic was exposed: %q", got)
	}
}

func TestSessionTurnRunnerPermissionAndUnknownRequests(t *testing.T) {
	fixture := newSessionACPFixture(t, "requests")
	turn := fixture.submit(t, "permission prompt")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn); err != nil {
		t.Fatal(err)
	}
	log := readFile(t, fixture.childLog)
	if !strings.Contains(log, `-32601`) || !strings.Contains(log, `"optionId":"always"`) {
		t.Fatalf("permission/request replies = %q", log)
	}
}

func TestSessionTurnRunnerBoundsFramesStderrAndCancellation(t *testing.T) {
	for _, scenario := range []string{"malformed", "oversized", "hang"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newSessionACPFixture(t, scenario)
			turn := fixture.submit(t, "bounded prompt")
			ctx := contextWithTurnDeadline(t)
			if scenario == "hang" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
			}
			if _, err := fixture.runner.Run(ctx, fixture.session, turn); err == nil {
				t.Fatal("bounded failure unexpectedly succeeded")
			}
			got, err := fixture.store.GetTurn(context.Background(), fixture.session.ID, turn.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != session.TurnFailed {
				t.Fatalf("turn state = %s, want failed", got.State)
			}
			wantCode := sessionACPProtocolError
			if scenario == "hang" {
				wantCode = sessionACPTimeoutError
			}
			if got.ErrorCode != wantCode {
				t.Fatalf("turn error code = %s, want %s", got.ErrorCode, wantCode)
			}
			if _, err := os.Stat(filepath.Join(fixture.private, "codex", "profiles", "work", "auth.json")); !os.IsNotExist(err) {
				t.Fatalf("projected credential remains after %s: %v", scenario, err)
			}
			if got := readFile(t, fixture.runtimeLog); !strings.Contains(got, "coop.run") {
				t.Fatalf("cleanup label missing after %s: %q", scenario, got)
			}
			if scenario == "hang" {
				if got := readFile(t, fixture.childLog); !strings.Contains(got, `"method":"session/cancel"`) {
					t.Fatalf("ACP cancellation notification missing: %q", got)
				}
			}
		})
	}
}

func TestSessionTurnRunnerBoundsStderrWithoutLosingResponse(t *testing.T) {
	fixture := newSessionACPFixture(t, "stderr")
	turn := fixture.submit(t, "bounded stderr")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if err != nil || result.State != session.TurnCompleted {
		t.Fatalf("bounded stderr turn = %+v, err=%v", result, err)
	}
}

func TestSessionTurnRunnerRejectsSourceSymlinkAndRefreshRequired(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		fixture := newSessionACPFixture(t, "normal")
		credential := filepath.Join(fixture.source, "codex", "profiles", "work", "auth.json")
		outside := filepath.Join(t.TempDir(), "auth.json")
		if err := os.Rename(credential, outside); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, credential); err != nil {
			t.Fatal(err)
		}
		turn := fixture.submit(t, "symlink")
		if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn); err == nil {
			t.Fatal("source symlink unexpectedly projected")
		}
		if _, err := os.Stat(filepath.Join(fixture.private, "codex", "profiles", "work", "auth.json")); !os.IsNotExist(err) {
			t.Fatalf("symlink failure left a projected credential: %v", err)
		}
	})
	t.Run("refresh required is renewed before projection", func(t *testing.T) {
		fixture := newSessionACPFixture(t, "normal")
		credential := filepath.Join(fixture.source, "codex", "profiles", "work", "auth.json")
		if err := os.WriteFile(credential, []byte(codexTestCredential(time.Now().Add(-time.Minute))), 0o600); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(2*time.Hour).Unix())))
			_, _ = fmt.Fprintf(w, `{"id_token":"identity","access_token":"x.%s.x","refresh_token":"rotated"}`, payload)
		}))
		defer server.Close()
		t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", server.URL)
		turn := fixture.submit(t, "expired")
		result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
		if err != nil || result.State != session.TurnCompleted {
			t.Fatalf("renewed turn = %+v, err=%v", result, err)
		}
		projectedLog := readFile(t, fixture.childLog)
		if strings.Contains(projectedLog, "rotated") || strings.Contains(projectedLog, "refresh") {
			t.Fatalf("refresh authority reached ACP child: %s", projectedLog)
		}
	})
}

func TestSessionTurnRunnerReplacesStaleProjectedCredential(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	profile := filepath.Join(fixture.private, "codex", "profiles", "work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "auth.json"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	turn := fixture.submit(t, "recover after crash")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(profile, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("projected credential remains after recovery: %v", err)
	}
}

func TestEnsurePrivateDirectoryRejectsIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "acp")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "acp", "session")
	if err := ensurePrivateDirectory(target); err == nil {
		t.Fatal("intermediate private-directory symlink was accepted")
	}
	if pathExists(filepath.Join(outside, "session")) {
		t.Fatal("private directory creation followed an intermediate symlink")
	}
}

func TestSessionACPProjectionTracksDestinationBeforeFailedWrite(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	profile := filepath.Join(fixture.private, "codex", "profiles", "work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(profile, "auth.json")
	if err := os.Mkdir(authPath, 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := agents.ParseTarget(fixture.session.Target)
	if err != nil {
		t.Fatal(err)
	}
	agent, ok := agents.Get(target.Provider)
	if !ok {
		t.Fatal("codex test agent is unavailable")
	}
	projection, err := fixture.runner.projectCredentials(fixture.session, target, agent, time.Now().Add(time.Hour))
	if err == nil || projection == nil {
		t.Fatalf("failed projection = %v, projection=%+v", err, projection)
	}
	found := false
	for _, path := range projection.files {
		if path == authPath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("failed destination was not recorded: %v", projection.files)
	}
}

func TestSessionACPProjectionCanOmitSharedEnvironmentAndMCP(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	bound := fixture.session
	bound.ProjectEnv = false
	bound.ProjectMCP = false
	target, err := agents.ParseTarget(bound.Target)
	if err != nil {
		t.Fatal(err)
	}
	agent, ok := agents.Get(target.Provider)
	if !ok {
		t.Fatal("codex test agent is unavailable")
	}
	projection, err := fixture.runner.projectCredentials(bound, target, agent, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projection.remove() })
	for _, name := range []string{"env", "mcp.json"} {
		if pathExists(filepath.Join(fixture.private, name)) {
			t.Fatalf("policy-disabled %s was projected", name)
		}
	}
	for _, path := range []string{
		filepath.Join(fixture.private, "INSTRUCTIONS.md"),
		filepath.Join(fixture.private, "codex", "profiles", "work", "auth.json"),
	} {
		if !pathExists(path) {
			t.Fatalf("required session projection is missing: %s", path)
		}
	}
}

func TestSessionTurnRunnerStartupCleanupPreservesNativeHistory(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	profile := filepath.Join(fixture.private, "codex", "profiles", "work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(profile, "auth.json")
	history := filepath.Join(profile, "native-history")
	instructions := filepath.Join(profile, "AGENTS.md")
	defaults := filepath.Join(fixture.private, "defaults")
	for path, data := range map[string]string{
		credential:   "stale credential",
		history:      "keep native history",
		instructions: "untrusted future instructions",
		defaults:     "codex=work\n",
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.runner.CleanupSession(context.Background(), fixture.session); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{credential, instructions, defaults} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("projected file %s remains after startup cleanup: %v", path, err)
		}
	}
	if got := readFile(t, history); got != "keep native history" {
		t.Fatalf("native history changed during cleanup: %q", got)
	}
}

func TestSessionTurnRunnerDoesNotReapBeforeChildStarts(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	turn := fixture.submit(t, "bad binding")
	bound := fixture.session
	bound.Workspace = filepath.Join(t.TempDir(), "not-the-fork")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), bound, turn); err == nil {
		t.Fatal("invalid workspace unexpectedly ran")
	}
	if _, err := os.Stat(fixture.runtimeLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation failure touched runtime cleanup: %v", err)
	}
}

// A completed turn outranks its janitorial proof. This test used to assert the
// opposite — that an unprovable service cleanup FAILS the turn — and production
// showed what that costs: every session_cleanup_error turn on 2026-08-07 was a
// finished multi-minute investigation destroyed over a slow `docker rm`, while
// the per-minute janitor stood ready to retry the same teardown anyway.
func TestSessionTurnRunnerCompletesTheTurnWhenCleanupFails(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	t.Setenv("COOP_TEST_SESSION_SERVICE_CLEANUP_FAIL", "1")
	turn := fixture.submit(t, "cleanup failure")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if err != nil {
		t.Fatalf("a finished answer was destroyed by its own cleanup: %v", err)
	}
	if result.State != session.TurnCompleted || result.AssistantMessage != "hello world" {
		t.Fatalf("turn after cleanup failure = %+v", result)
	}
	got, err := fixture.store.GetTurn(context.Background(), fixture.session.ID, turn.ID)
	if err != nil || got.State != session.TurnCompleted || got.AssistantMessage != "hello world" {
		t.Fatalf("stored turn after cleanup failure = state %q message %q, err=%v", got.State, got.AssistantMessage, err)
	}
}

func TestSessionTurnRunnerPersistsUsageUpdateCost(t *testing.T) {
	fixture := newSessionACPFixture(t, "usage")
	turn := fixture.submit(t, "measure this turn")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Usage.CostRecorded || result.Usage.CostUSD != 0.375 ||
		result.Usage.InputTokens != 1200 || result.Usage.OutputTokens != 34 {
		t.Fatalf("completed usage = %+v", result.Usage)
	}
}

// A turn that actually failed still reports the cleanup failure — with its
// cause, not the bare constant that made a slow daemon and a broken compose
// project read identically.
func TestSessionTurnRunnerFailedTurnCarriesTheCleanupCause(t *testing.T) {
	fixture := newSessionACPFixture(t, "hang")
	t.Setenv("COOP_TEST_SESSION_SERVICE_CLEANUP_FAIL", "1")
	turn := fixture.submit(t, "hang then fail cleanup")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := fixture.runner.Run(ctx, fixture.session, turn)
	if err == nil {
		t.Fatal("hung turn unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "session services cleanup failed:") {
		t.Fatalf("cleanup failure lost its cause: %v", err)
	}
}

func TestSessionRunIDFromEnv(t *testing.T) {
	t.Setenv("COOP_SESSION_RUN_ID", "not-a-session-run")
	if got := RunIDFromEnv(); got != "" {
		t.Fatalf("invalid session run id = %q", got)
	}
	want := sessionTurnRunID("session", "turn")
	t.Setenv("COOP_SESSION_RUN_ID", want)
	if got := RunIDFromEnv(); got != want {
		t.Fatalf("session run id = %q, want %q", got, want)
	}
}

func TestSessionACPChildEnvironmentForwardsOnlyResolvedBoxSettings(t *testing.T) {
	t.Setenv("COOP_CONFIG_DIR", t.TempDir())
	t.Setenv("COOP_EGRESS", "none")
	t.Setenv("COOP_MEMORY", "2g")
	t.Setenv("COOP_RUN_ARGS", "--privileged")
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	cfg := config.Load()
	got := map[string]string{}
	companions := []session.CompanionRepository{{
		Name: "topology", Repository: "/source",
		Workspace: "/snapshot", BaseCommit: strings.Repeat("a", 40),
	}}
	for _, item := range sessionACPChildEnvironment(
		"/repo", companions, true, "/private", "session-000000000000000000000000",
		cfg, "docker",
	) {
		key, value, _ := strings.Cut(item, "=")
		got[key] = value
	}
	for key, want := range map[string]string{
		"COOP_REPO":                         "/repo",
		"COOP_CONFIG_DIR":                   "/private",
		"COOP_HOMES":                        "1",
		"COOP_RUNTIME":                      "docker",
		"COOP_EGRESS":                       "none",
		"COOP_MEMORY":                       "2g",
		"COOP_SESSION_REPOSITORY_READ_ONLY": "1",
	} {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q", key, got[key], want)
		}
	}
	var gotCompanions []session.CompanionRepository
	if err := json.Unmarshal(
		[]byte(got["COOP_SESSION_COMPANIONS"]), &gotCompanions,
	); err != nil || len(gotCompanions) != 1 ||
		gotCompanions[0] != companions[0] {
		t.Fatalf("companion environment = %+v, %v", gotCompanions, err)
	}
	for _, key := range []string{"COOP_RUN_ARGS", "OPENAI_API_KEY", "COOP_MCP_FILE"} {
		if _, ok := got[key]; ok {
			t.Fatalf("unsafe ambient %s reached child environment", key)
		}
	}

	// Ambient COOP_* input is stripped; only the persisted session bit may add
	// this mount authority to the trusted child environment.
	for _, item := range sessionACPChildEnvironment(
		"/repo", companions, false, "/private", "session-000000000000000000000000",
		cfg, "docker",
	) {
		if strings.HasPrefix(item, "COOP_SESSION_REPOSITORY_READ_ONLY=") {
			t.Fatalf("ambient read-only flag overrode the persisted session authority: %q", item)
		}
	}
}

type sessionACPFixture struct {
	t          *testing.T
	store      *session.Store
	session    session.Session
	runner     *sessionTurnRunner
	repo       string
	source     string
	private    string
	childLog   string
	envLog     string
	runtimeLog string
}

func codexTestCredential(expires time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expires.Unix())))
	return `{"auth_mode":"chatgpt","tokens":{"id_token":"identity","access_token":"x.` + payload + `.x","refresh_token":"refresh"},"last_refresh":"2026-07-26T00:00:00Z"}`
}

func claudeTestCredential(expires time.Time) string {
	return fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":"access","refreshToken":"refresh","expiresAt":%d,"scopes":["user:inference"]}}`,
		expires.UnixMilli(),
	)
}

// writeSessionTestCredential signs the fixture's account in for whichever provider the target
// names, in that provider's own on-disk shape — what projectCredentials reads, renews and projects.
func writeSessionTestCredential(t *testing.T, source, target string) {
	t.Helper()
	parsed, err := agents.ParseTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(source, parsed.Provider, "profiles", parsed.Account())
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	name, body := "auth.json", codexTestCredential(time.Now().Add(2*time.Hour))
	if parsed.Provider == "claude" {
		name, body = ".credentials.json", claudeTestCredential(time.Now().Add(2*time.Hour))
	}
	if err := os.WriteFile(filepath.Join(profile, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newSessionACPFixture builds a signed-in session on codex@work, or on the one target given.
func newSessionACPFixture(t *testing.T, scenario string, target ...string) *sessionACPFixture {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "shared-agents")
	sessionTarget := "codex@work"
	if len(target) > 0 && target[0] != "" {
		sessionTarget = target[0]
	}
	writeSessionTestCredential(t, source, sessionTarget)
	for name, body := range map[string]string{
		"env": "EMISAR_TOKEN=observe-only\n",
		// The production shape: a remote server the box authenticates from its env file, which
		// only an agent holding that file can resolve for itself.
		"mcp.json":        `{"mcpServers":{"emisar":{"type":"http","url":"https://example.invalid/mcp","bearer_token_env_var":"EMISAR_TOKEN"}}}`,
		"INSTRUCTIONS.md": "Investigate before changing code.\n",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := forkspace.Workspace(repo, "fork")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "state")
	store, err := session.Open(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sess, err := store.CreateSession(context.Background(), "create", session.CreateSessionRequest{
		Target: sessionTarget, Policy: "policy", Repository: repo, Workspace: workspace, ForkName: "fork", BaseCommit: strings.Repeat("a", 40),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolvedStateRoot, err := filepath.EvalSymlinks(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(resolvedStateRoot, "acp", sess.ID)
	childLog := filepath.Join(root, "child.log")
	envLog := filepath.Join(root, "env.log")
	runtimeLog := filepath.Join(root, "runtime.log")
	t.Setenv("COOP_TEST_SESSION_CHILD", "1")
	t.Setenv("COOP_TEST_SESSION_SCENARIO", scenario)
	t.Setenv("COOP_TEST_SESSION_CHILD_LOG", childLog)
	t.Setenv("COOP_TEST_SESSION_ENV_LOG", envLog)
	t.Setenv("COOP_CONFIG_DIR", filepath.Join(root, "ambient-config"))
	t.Setenv("COOP_REPO", filepath.Join(root, "ambient-repo"))
	t.Setenv("COOP_MCP_FILE", filepath.Join(root, "ambient-mcp.json"))
	t.Setenv("OPENAI_API_KEY", "secret")
	runtimePath := filepath.Join(root, "fake-runtime")
	runtimeScript := `#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_SESSION_RUNTIME_LOG"
if [ "$1" = ps ]; then
	case "$*" in
		*com.docker.compose.project=*)
			[ "$COOP_TEST_SESSION_SERVICE_CLEANUP_FAIL" = 1 ] && exit 41
			;;
	esac
	echo fake-container
fi
exit 0
`
	if err := os.WriteFile(runtimePath, []byte(runtimeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_SESSION_RUNTIME_LOG", runtimeLog)
	runner := newSessionTurnRunner(&config.Config{ConfigDir: source}, stateRoot, store,
		runtime.Runtime{Name: runtimePath}, os.Args[0], func(_ string, _ ...string) *exec.Cmd {
			return exec.Command(os.Args[0], "-test.run=TestSessionACPChildHelper")
		})
	return &sessionACPFixture{t: t, store: store, session: sess, runner: runner, repo: repo, source: source,
		private: private, childLog: childLog, envLog: envLog, runtimeLog: runtimeLog}
}

// signIn adds another signed-in credential to the shared source config, so a ladder has a rung
// to rotate onto.
func (f *sessionACPFixture) signIn(t *testing.T, agent, credential string) {
	t.Helper()
	dir := filepath.Join(f.source, agent, "profiles", credential)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := codexTestCredential(time.Now().Add(2 * time.Hour))
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ladderContext supplies the policy ladder the service would attach to a turn's context.
func ladderContext(t *testing.T, parent context.Context, values ...string) context.Context {
	t.Helper()
	return context.WithValue(parent, sessionTargetLadderContextKey{}, mustTargets(values...))
}

func (f *sessionACPFixture) submit(t *testing.T, prompt string) session.Turn {
	return f.submitRequest(t, session.SubmitTurnRequest{Prompt: prompt})
}

func (f *sessionACPFixture) submitArtifacts(
	t *testing.T,
	prompt string,
	artifacts []session.InputArtifact,
) session.Turn {
	return f.submitRequest(t, session.SubmitTurnRequest{Prompt: prompt, Artifacts: artifacts})
}

func (f *sessionACPFixture) submitContract(t *testing.T, prompt string) session.Turn {
	schema := json.RawMessage(`{"type":"object","properties":{"reply":{"type":"string"}},"required":["reply"],"additionalProperties":false}`)
	digest := sha256.Sum256(schema)
	return f.submitRequest(t, session.SubmitTurnRequest{
		Prompt: prompt,
		OutputContract: &session.OutputContract{
			JSONSchema: schema,
			SHA256:     hex.EncodeToString(digest[:]),
		},
	})
}

func (f *sessionACPFixture) submitSemanticContract(t *testing.T, prompt string) session.Turn {
	schema := json.RawMessage(`{"type":"object","properties":{"reply":{"type":"string"}},"required":["reply"],"additionalProperties":false}`)
	digest := sha256.Sum256(schema)
	return f.submitRequest(t, session.SubmitTurnRequest{
		Prompt: prompt,
		OutputContract: &session.OutputContract{
			JSONSchema: schema, SHA256: hex.EncodeToString(digest[:]),
			RequireSemanticValidation: true,
		},
	})
}

// submitFromRung admits a turn the caller requires to be delivered no lower than
// the named rung of the policy ladder — Responder's escalation path.
func (f *sessionACPFixture) submitFromRung(t *testing.T, prompt string, floor int) session.Turn {
	return f.submitRequest(t, session.SubmitTurnRequest{Prompt: prompt, MinTargetIndex: floor})
}

// submitRequest admits one turn against the session's current revision and leases
// it, which is the state Run expects to be handed.
func (f *sessionACPFixture) submitRequest(t *testing.T, req session.SubmitTurnRequest) session.Turn {
	t.Helper()
	sess, err := f.store.GetSession(context.Background(), f.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.SessionID, req.ExpectedRevision = sess.ID, sess.Revision
	_, err = f.store.SubmitTurn(context.Background(), "turn-"+req.Prompt, req)
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := f.store.LeaseNextTurn(context.Background(), sess.ID)
	if err != nil || !ok {
		t.Fatalf("lease turn = %v, %v", leased, err)
	}
	return leased
}

func contextWithTurnDeadline(t *testing.T) context.Context {
	return contextWithTurnTimeout(t, 5*time.Second)
}

func contextWithTurnTimeout(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

func readSessionACPLog(t *testing.T, path string) []string {
	t.Helper()
	data := readFile(t, path)
	var methods []string
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if line == "" {
			continue
		}
		var frame struct {
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(line), &frame) == nil && frame.Method != "" {
			methods = append(methods, frame.Method)
		}
	}
	return methods
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func TestAccumulateSessionACPUpdateDecodesContentByUpdateType(t *testing.T) {
	var assistant []byte
	toolCall := json.RawMessage(`{
		"sessionId":"native-1",
		"update":{
			"sessionUpdate":"tool_call",
			"toolCallId":"tool-1",
			"content":[{"type":"terminal","terminalId":"terminal-1"}]
		}
	}`)
	if err := accumulateSessionACPUpdate(toolCall, "native-1", &assistant); err != nil {
		t.Fatalf("valid tool-call update = %v", err)
	}
	if len(assistant) != 0 {
		t.Fatalf("tool-call content leaked into assistant message: %q", assistant)
	}

	message := json.RawMessage(`{
		"sessionId":"native-1",
		"update":{
			"sessionUpdate":"agent_message_chunk",
			"content":{"type":"text","text":"verified result"}
		}
	}`)
	if err := accumulateSessionACPUpdate(message, "native-1", &assistant); err != nil {
		t.Fatalf("agent message update = %v", err)
	}
	if got, want := string(assistant), "verified result"; got != want {
		t.Fatalf("assistant message = %q, want %q", got, want)
	}
}

// sessionSlowStreamFrames is how many prompt-phase frames the "slow-stream"
// child produces in total, counting the response that ends it.
const sessionSlowStreamFrames = 7

func TestSessionACPChildHelper(t *testing.T) {
	if os.Getenv("COOP_TEST_SESSION_CHILD") != "1" {
		return
	}
	logPath := os.Getenv("COOP_TEST_SESSION_CHILD_LOG")
	envPath := os.Getenv("COOP_TEST_SESSION_ENV_LOG")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(2)
	}
	defer log.Close()
	env, err := os.OpenFile(envPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(2)
	}
	var projected []string
	for _, name := range []string{"env", "mcp.json", "INSTRUCTIONS.md"} {
		if info, err := os.Stat(filepath.Join(os.Getenv("COOP_CONFIG_DIR"), name)); err == nil && info.Mode().IsRegular() {
			projected = append(projected, name)
		}
	}
	fmt.Fprintf(env, "config=%s repo=%s mcp=%s openai=%s run=%s files=%s\n", os.Getenv("COOP_CONFIG_DIR"), os.Getenv("COOP_REPO"), os.Getenv("COOP_MCP_FILE"), os.Getenv("OPENAI_API_KEY"), os.Getenv("COOP_SESSION_RUN_ID"), strings.Join(projected, ","))
	env.Close()
	privateNative := filepath.Join(os.Getenv("COOP_CONFIG_DIR"), "codex", "profiles", "work", "native-history")
	_ = os.WriteFile(privateNative, []byte("native"), 0o600)

	scenario := os.Getenv("COOP_TEST_SESSION_SCENARIO")
	if scenario == "missing-image" {
		_, _ = os.Stderr.WriteString("image \"coop-box\" not built - run 'coop build'\n")
		os.Exit(7)
	}
	reader := bufio.NewScanner(os.Stdin)
	reader.Buffer(make([]byte, 1024), sessionACPFrameLimit)
	writer := bufio.NewWriter(os.Stdout)
	send := func(value any) {
		data, _ := json.Marshal(value)
		_, _ = writer.Write(append(data, '\n'))
		_ = writer.Flush()
	}
	pendingPermission := false
	var promptID json.RawMessage
	promptCount := 0
	for reader.Scan() {
		line := reader.Bytes()
		_, _ = log.Write(append(append([]byte(nil), line...), '\n'))
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				SessionID string `json:"sessionId"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &frame) != nil {
			continue
		}
		if frame.Method == "" && pendingPermission {
			pendingPermission = false
			if scenario == "requests" {
				send(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]any{"stopReason": "end_turn"}})
			}
			continue
		}
		switch frame.Method {
		case "initialize":
			if scenario == "initialize-fail" {
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": map[string]any{"code": -32000, "message": "initialize failed"}})
			} else {
				send(map[string]any{
					"jsonrpc": "2.0", "id": frame.ID,
					"result": map[string]any{
						"protocolVersion": 1,
						"agentCapabilities": map[string]any{
							"promptCapabilities": map[string]any{
								"image": true, "embeddedContext": true,
							},
						},
					},
				})
			}
		case "session/new":
			send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"sessionId": "native-1"}})
		case "session/load":
			if scenario == "load-fail" {
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": map[string]any{"code": -32000, "message": "load failed"}})
			} else {
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "old history"}}}})
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{}})
			}
		case "session/prompt":
			promptCount++
			switch scenario {
			case "invalid-contract-once", "invalid-contract-always", "valid-contract":
				message := `{"reply":"valid"}`
				if scenario != "valid-contract" && (scenario == "invalid-contract-always" || promptCount == 1) {
					message = `{"reply":"invalid"}}`
				}
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "assistant_message_chunk", "content": map[string]string{"type": "text", "text": message}}}})
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{
					"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 10, "outputTokens": 2},
				}})
			case "usage":
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": frame.Params.SessionID,
					"update": map[string]any{"sessionUpdate": "usage_update", "used": 123, "size": 200000,
						"cost": map[string]any{"amount": 0.375, "currency": "USD"}},
				}})
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "assistant_message_chunk", "content": map[string]string{"type": "text", "text": "measured"}}}})
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{
					"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 1200, "outputTokens": 34},
				}})
			case "large-tool-output":
				chunk := strings.Repeat("e", 1<<20)
				for i := 0; i < 5; i++ {
					send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
						"sessionId": frame.Params.SessionID,
						"update": map[string]any{
							"sessionUpdate": "tool_call_update",
							"toolCallId":    "large-tool",
							"content":       []any{map[string]any{"type": "content", "content": map[string]string{"type": "text", "text": chunk}}},
						},
					}})
				}
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "assistant_message_chunk", "content": map[string]string{"type": "text", "text": "investigation complete"}}}})
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"stopReason": "end_turn"}})
			case "image-output":
				image := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, sessionACPTranscriptLimit+1024)...)
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "assistant_message_chunk", "content": map[string]string{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(image)}}}})
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "assistant_message_chunk", "content": map[string]string{"type": "text", "text": "chart attached"}}}})
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"stopReason": "end_turn"}})
			case "tool-image-output":
				image := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, sessionACPTranscriptLimit+1024)...)
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{
					"sessionUpdate": "tool_call_update", "toolCallId": "image-tool-1",
					"content": []any{map[string]any{"type": "content", "content": map[string]string{
						"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(image),
					}}},
				}}})
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "assistant_message_chunk", "content": map[string]string{"type": "text", "text": "chart attached"}}}})
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"stopReason": "end_turn"}})
			case "rate-limited", "rate-limited-once":
				// "rate-limited-once" limits only the first child, so the turn's retry lands on
				// the next rung; the marker survives the process because each rung is a new one.
				limited := true
				if marker := os.Getenv("COOP_TEST_SESSION_LIMIT_MARKER"); scenario == "rate-limited-once" && marker != "" {
					if _, err := os.Stat(marker); err == nil {
						limited = false
					} else {
						_ = os.WriteFile(marker, []byte("1"), 0o600)
					}
				}
				if limited {
					send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "error": map[string]any{
						"code":    -32000,
						"message": fmt.Sprintf("Claude AI usage limit reached|%d", time.Now().Add(time.Hour).Unix()),
					}})
					continue
				}
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "assistant_message_chunk", "content": map[string]string{"type": "text", "text": "rotated answer"}}}})
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"stopReason": "end_turn"}})
			case "slow-stream":
				// A provider CLI retrying 429s inside itself: the transport keeps
				// moving and nothing it carries is narratable — no tool calls, no
				// plan, no thoughts, and no ACP-level rejection for the ladder to see.
				for range sessionSlowStreamFrames - 1 {
					send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "."}}}})
				}
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"stopReason": "end_turn"}})
			case "limit-prose":
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "assistant_message_chunk", "content": map[string]string{"type": "text", "text": "the runbook says: Claude AI usage limit reached|2000000000"}}}})
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"stopReason": "end_turn"}})
			case "malformed":
				_, _ = writer.WriteString("not-json\n")
				_ = writer.Flush()
			case "oversized":
				_, _ = writer.WriteString(strings.Repeat("x", sessionACPFrameLimit+1) + "\n")
				_ = writer.Flush()
			case "stderr":
				_, _ = os.Stderr.WriteString(strings.Repeat("e", 1<<20))
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"stopReason": "end_turn"}})
			case "hang":
				continue
			case "requests":
				promptID = append(promptID[:0], frame.ID...)
				send(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "fs/read_text_file", "params": map[string]any{}})
				pendingPermission = true
				// The next frame is the fs response. Ask permission after it is rejected.
				for reader.Scan() {
					line = reader.Bytes()
					_, _ = log.Write(append(append([]byte(nil), line...), '\n'))
					pendingPermission = false
					send(map[string]any{"jsonrpc": "2.0", "id": 8, "method": "session/request_permission", "params": map[string]any{"options": []map[string]string{{"optionId": "reject", "kind": "reject_once"}, {"optionId": "once", "kind": "allow_once"}, {"optionId": "always", "kind": "allow_always"}}}})
					pendingPermission = true
					break
				}
			default:
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "tool_call", "content": map[string]string{"type": "text", "text": "ignored"}}}})
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "assistant_message_chunk", "content": map[string]string{"type": "text", "text": "hello "}}}})
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": frame.Params.SessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "world"}}}})
				send(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"stopReason": "end_turn"}})
			}
		}
	}
}
