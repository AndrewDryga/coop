package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/session"
)

// recordChildArgv makes the fixture's child launch remember the coop arguments the daemon built,
// while still running the scripted ACP child.
func recordChildArgv(fixture *sessionACPFixture) *[]string {
	var argv []string
	fixture.runner.command = func(_ string, args ...string) *exec.Cmd {
		argv = append([]string(nil), args...)
		return exec.Command(os.Args[0], "-test.run=TestSessionACPChildHelper")
	}
	return &argv
}

// sessionNewParams returns the params of the one session/new request the child logged.
func sessionNewParams(t *testing.T, childLog string) map[string]any {
	return sessionNewParamsForMethod(t, childLog, "session/new")
}

func sessionNewParamsForMethod(t *testing.T, childLog, method string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(readFile(t, childLog)), "\n") {
		var frame struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal([]byte(line), &frame) == nil && frame.Method == method {
			return frame.Params
		}
	}
	t.Fatalf("no %s request was sent", method)
	return nil
}

// promptText returns the text content of the one session/prompt request the child logged.
func promptText(t *testing.T, childLog string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(readFile(t, childLog)), "\n") {
		var frame struct {
			Method string `json:"method"`
			Params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &frame) == nil && frame.Method == "session/prompt" && len(frame.Params.Prompt) > 0 {
			return frame.Params.Prompt[0].Text
		}
	}
	t.Fatal("no session/prompt request was sent")
	return ""
}

// claudeCodeOptions digs the adapter's option block out of a session/new `_meta`.
func claudeCodeOptions(t *testing.T, params map[string]any) map[string]any {
	t.Helper()
	meta, _ := params["_meta"].(map[string]any)
	claudeCode, _ := meta["claudeCode"].(map[string]any)
	options, _ := claudeCode["options"].(map[string]any)
	if options == nil {
		t.Fatalf("session/new carried no claudeCode options: %v", params)
	}
	return options
}

// A bare session's child is the plain adapter launch under --bare, with the run receipt and no
// repository in its environment; its session/new names the box's scratch cwd and carries the
// adapter's no-tools meta; its prompt announces no output directory; and the native session it
// ran in is never bound, because the box that held it is gone.
func TestABareSessionLaunchesTheAdapterWithoutAWorkspace(t *testing.T) {
	fixture := newSessionACPFixtureUnder(t, "normal", "claude@work", agents.ModeBare)
	if fixture.session.Workspace != "" || fixture.session.Mode != "bare" {
		t.Fatalf("bare fixture = %+v", fixture.session)
	}
	argv := recordChildArgv(fixture)
	turn := fixture.submit(t, "route this")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if err != nil || result.State != session.TurnCompleted || result.AssistantMessage != "hello world" {
		t.Fatalf("bare turn = %+v, %v", result, err)
	}
	if want := []string{"acp", "claude@work", "--bare"}; !slices.Equal(*argv, want) {
		t.Fatalf("bare child argv = %q, want %q", *argv, want)
	}
	env := readFile(t, fixture.envLog)
	if !strings.Contains(env, "repo= ") || !strings.Contains(env, "run="+sessionTurnRunID(fixture.session.ID, turn.ID)) || !strings.Contains(env, "ro= authority=") {
		t.Fatalf("bare child environment = %q", env)
	}
	params := sessionNewParams(t, fixture.childLog)
	if params["cwd"] != box.BareWorkdir {
		t.Fatalf("bare session/new cwd = %v, want %s", params["cwd"], box.BareWorkdir)
	}
	options := claudeCodeOptions(t, params)
	if tools, ok := options["tools"].([]any); !ok || len(tools) != 0 {
		t.Fatalf("bare session/new tools = %v, want an empty array", options["tools"])
	}
	if sources, _ := options["settingSources"].([]any); len(sources) != 1 || sources[0] != "user" || options["strictMcpConfig"] != true {
		t.Fatalf("bare session/new options = %v", options)
	}
	if meta := params["_meta"].(map[string]any); !strings.Contains(meta["systemPrompt"].(map[string]any)["append"].(string), "tool set is empty") {
		t.Fatalf("bare session/new system prompt = %v", meta["systemPrompt"])
	}
	if servers, _ := params["mcpServers"].([]any); len(servers) != 0 {
		t.Fatalf("bare session/new handed MCP servers: %v", params["mcpServers"])
	}
	if text := promptText(t, fixture.childLog); strings.Contains(text, "<coop-output>") || text != "route this" {
		t.Fatalf("bare prompt = %q", text)
	}
	stored, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil || stored.NativeSessionID != "" {
		t.Fatalf("bare session bound a native session nothing can load: %+v, %v", stored, err)
	}
	if entries, _ := os.ReadDir(fixture.repo); len(entries) != 0 {
		t.Fatalf("bare session touched the repository directory: %v", entries)
	}
	// The child owns a host seed directory it removes only when its run returns, so it gets the
	// bounded stop grace a filtered child gets, never the quarter-second signal.
	process, err := fixture.runner.startChildWithRunID(contextWithTurnDeadline(t), fixture.session, sessionTurnRunID(fixture.session.ID, "grace"), fixture.private)
	if err != nil {
		t.Fatal(err)
	}
	defer process.stop()
	if !process.restricted || process.stopGrace != sessionACPRestrictedStopGrace || process.cwd != box.BareWorkdir {
		t.Fatalf("bare child process = restricted:%v grace:%s cwd:%q", process.restricted, process.stopGrace, process.cwd)
	}
}

// A readonly session's child fronts its fork under --readonly: the fork is proven the session's
// own exactly as before, the legacy writable output root is neither prepared nor announced, the
// session/new keeps the fork's in-box cwd and carries the adapter's extension-free meta with the tool
// set untouched, and no native session is bound.
func TestAReadOnlySessionLaunchesItsForkWithoutAnOutputRoot(t *testing.T) {
	fixture := newSessionACPFixtureUnder(t, "normal", "claude@work", agents.ModeReadOnly)
	argv := recordChildArgv(fixture)
	turn := fixture.submit(t, "inspect this")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if err != nil || result.State != session.TurnCompleted {
		t.Fatalf("readonly turn = %+v, %v", result, err)
	}
	if want := []string{"fork", "fork", "acp", "claude@work", "--readonly"}; !slices.Equal(*argv, want) {
		t.Fatalf("readonly child argv = %q, want %q", *argv, want)
	}
	if env := readFile(t, fixture.envLog); !strings.Contains(env, "ro= authority=") || !strings.Contains(env, "repo="+fixture.repo+" ") {
		t.Fatalf("readonly child environment = %q", env)
	}
	if _, err := os.Lstat(filepath.Join(fixture.session.Workspace, sessionOutputRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readonly session prepared a writable output root: %v", err)
	}
	params := sessionNewParams(t, fixture.childLog)
	wantCWD := box.BareWorkdir
	if params["cwd"] != wantCWD {
		t.Fatalf("readonly session/new cwd = %v, want in-box path %s", params["cwd"], wantCWD)
	}
	options := claudeCodeOptions(t, params)
	if _, ok := options["tools"]; ok || options["strictMcpConfig"] != true {
		t.Fatalf("readonly session/new options = %v", options)
	}
	if meta := params["_meta"].(map[string]any); meta["systemPrompt"] != nil {
		t.Fatalf("readonly session/new appended a system prompt: %v", meta)
	}
	if text := promptText(t, fixture.childLog); strings.Contains(text, "<coop-output>") {
		t.Fatalf("readonly prompt announces an output directory: %q", text)
	}
	stored, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil || stored.NativeSessionID != "" {
		t.Fatalf("readonly session bound a native session nothing can load: %+v, %v", stored, err)
	}
}

// The output note names this turn's own folder. Ahead of the caller's prompt
// it ended the provider's prefix cache before the caller's instructions, so a
// caller whose instructions come first, as Ryker's do, paid for them in full
// on every call (2026-09-27: 12,160 cached tokens every time, all Codex's own).
func TestANormalTurnPromptBeginsWithTheCallersWords(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	turn := fixture.submit(t, "inspect this")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if err != nil || result.State != session.TurnCompleted {
		t.Fatalf("normal turn = %+v, %v", result, err)
	}
	text := promptText(t, fixture.childLog)
	if !strings.HasPrefix(text, "inspect this\n\n<coop-output>") || !strings.HasSuffix(text, "</coop-output>") {
		t.Fatalf("normal prompt = %q", text)
	}
}

func TestNormalRemoteSessionsAnnounceOneBoxCWDForPrivateForks(t *testing.T) {
	var workspaces []string
	for range 2 {
		fixture := newSessionACPFixture(t, "normal")
		workspaces = append(workspaces, fixture.session.Workspace)
		turn := fixture.submit(t, "inspect this")
		result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
		if err != nil || result.State != session.TurnCompleted {
			t.Fatalf("normal turn = %+v, %v", result, err)
		}
		if got := sessionNewParams(t, fixture.childLog)["cwd"]; got != box.BareWorkdir {
			t.Fatalf("normal session/new cwd = %v, want %s", got, box.BareWorkdir)
		}
	}
	if workspaces[0] == workspaces[1] {
		t.Fatalf("remote sessions shared a host fork: %q", workspaces[0])
	}
}

// Both profiles use continuous canonical renewal. A turn need not fit within
// one access token's lifetime, and the child never receives that token.
func TestARestrictedSessionNeedsOnlyCanonicalStartupReadiness(t *testing.T) {
	for _, mode := range []agents.ExecutionMode{agents.ModeBare, agents.ModeNormal} {
		fixture := newSessionACPFixtureUnder(t, "normal", "claude@work", mode)
		writeSessionTestCredential(t, fixture.source, "claude@work", time.Now().Add(30*time.Minute))
		turn := fixture.submit(t, "horizon")
		result, err := fixture.runner.Run(contextWithTurnTimeout(t, 2*time.Hour), fixture.session, turn)
		if err != nil || result.State != session.TurnCompleted {
			t.Fatalf("%s turn on a renewable 30-minute token = %+v, %v", mode, result, err)
		}
	}
}

// Filtered and open runs share the same startup horizon; neither prematurely
// consumes a refresh grant just because its turn could last longer.
func TestAFilteredGrokSessionDoesNotRefreshForTheWholeTurn(t *testing.T) {
	for _, network := range []egress.Mode{egress.Open, egress.Filtered} {
		t.Run(string(network), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "renewed-access", "refresh_token": "rotated-refresh", "expires_in": 21600})
			}))
			defer server.Close()
			t.Setenv("GROK_REFRESH_TOKEN_URL_OVERRIDE", server.URL)
			fixture := newSessionACPFixtureOn(t, "normal", "grok@work", agents.ModeNormal, network)
			writeSessionTestCredential(t, fixture.source, "grok@work", time.Now().Add(30*time.Minute))
			before := readFile(t, filepath.Join(fixture.source, "grok", "credentials", "work", "authority.json"))
			turn := fixture.submit(t, "renew")
			result, err := fixture.runner.Run(contextWithTurnTimeout(t, 2*time.Hour), fixture.session, turn)
			stored := readFile(t, filepath.Join(fixture.source, "grok", "credentials", "work", "authority.json"))
			if err != nil || result.State != session.TurnCompleted {
				t.Fatalf("%s turn on a 30-minute renewable login = %+v, %v", network, result, err)
			}
			if requests.Load() != 0 || stored != before {
				t.Fatalf("%s turn unnecessarily renewed (%d requests)", network, requests.Load())
			}
		})
	}
}

// A restricted session's schema repair cannot re-prompt a native session (the box is gone), so
// the second attempt regenerates from the admitted prompt and contract in a fresh session — and
// the turn still completes on the valid candidate.
func TestARestrictedSessionRegeneratesAnInvalidStructuredResultFromScratch(t *testing.T) {
	fixture := newSessionACPFixtureUnder(t, "invalid-contract-once", "claude@work", agents.ModeBare)
	turn := fixture.submitContract(t, "route this")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if err != nil || result.State != session.TurnCompleted || result.AssistantMessage != `{"reply":"valid"}` {
		t.Fatalf("repaired bare turn = %+v, %v", result, err)
	}
	methods := readSessionACPLog(t, fixture.childLog)
	if countStrings(methods, "session/new") != 2 || countStrings(methods, "session/load") != 0 {
		t.Fatalf("restricted repair methods = %v, want two fresh sessions and no load", methods)
	}
	var prompts []string
	for _, line := range strings.Split(strings.TrimSpace(readFile(t, fixture.childLog)), "\n") {
		var frame struct {
			Method string `json:"method"`
			Params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &frame) == nil && frame.Method == "session/prompt" {
			prompts = append(prompts, frame.Params.Prompt[0].Text)
		}
	}
	if len(prompts) != 2 || prompts[0] != prompts[1] || !strings.Contains(prompts[1], "route this") || strings.Contains(prompts[1], "previous final response") {
		t.Fatalf("restricted repair prompts = %q", prompts)
	}
}

// A bare session's runtime receipt is reaped by the run label alone: there is no fork whose
// registry could name the box, and no service or workspace to touch. The receipt is retired
// only after the runtime reported the removal, and a foreign receipt is still refused.
func TestABareSessionInterruptedTurnIsReapedByItsRunLabel(t *testing.T) {
	fixture := newSessionACPFixtureUnder(t, "normal", "claude@work", agents.ModeBare)
	turn := fixture.submit(t, "reap me")
	runID := sessionTurnRunID(fixture.session.ID, turn.ID)
	if err := fixture.store.BindTurnRuntime(context.Background(), fixture.session.ID, turn.ID, "", runID); err != nil {
		t.Fatal(err)
	}
	turn.RuntimeRunID = runID
	if err := fixture.runner.ReapInterruptedTurn(context.Background(), fixture.session, turn); err != nil {
		t.Fatal(err)
	}
	if log := readFile(t, fixture.runtimeLog); !strings.Contains(log, box.LabelRun+"="+runID) {
		t.Fatalf("bare reap did not remove by the run label: %s", log)
	}
	reaped, err := fixture.store.GetTurn(context.Background(), fixture.session.ID, turn.ID)
	if err != nil || reaped.RuntimeRunID != "" {
		t.Fatalf("bare receipt was not retired: %+v, %v", reaped, err)
	}
	foreign := turn
	foreign.RuntimeRunID = sessionTurnRunID("another-session", "another-turn")
	if err := fixture.runner.ReapInterruptedTurn(context.Background(), fixture.session, foreign); err == nil {
		t.Fatal("a foreign receipt was reaped on a bare session")
	}
	// The whole-session cleanups a close and the janitor run must not fail on the missing
	// workspace either.
	if err := fixture.runner.CleanupSession(context.Background(), fixture.session); err != nil {
		t.Fatalf("bare CleanupSession: %v", err)
	}
	if err := fixture.runner.CleanupClosedSession(context.Background(), fixture.session); err != nil {
		t.Fatalf("bare CleanupClosedSession: %v", err)
	}
}

// A bare turn that runs out of time tears its box down by the same label, and the cancellation
// reaches the child as a session/cancel before the process is stopped.
func TestABareSessionCancelledTurnTearsDownItsBox(t *testing.T) {
	fixture := newSessionACPFixtureUnder(t, "hang", "claude@work", agents.ModeBare)
	turn := fixture.submit(t, "hang")
	result, err := fixture.runner.Run(hangTimeout(t, fixture.childLog, 200*time.Millisecond), fixture.session, turn)
	if err == nil || result.State != session.TurnFailed {
		t.Fatalf("timed-out bare turn = %+v, %v", result, err)
	}
	if methods := readSessionACPLog(t, fixture.childLog); !slices.Contains(methods, "session/cancel") {
		t.Fatalf("bare cancellation never reached the child: %v", methods)
	}
	if log := readFile(t, fixture.runtimeLog); !strings.Contains(log, box.LabelRun+"="+sessionTurnRunID(fixture.session.ID, turn.ID)) {
		t.Fatalf("cancelled bare box was not removed by its run label: %s", log)
	}
}
