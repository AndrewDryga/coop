package sessionsvc

import (
	"context"
	"encoding/json"
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

// A job with repository_read_only runs in normal mode: its child is the same fork launch as any
// other session's, with no extra flag, and only the environment bit that mounts the fork read-only
// tells them apart. It still gets a writable output root, kept in the daemon's private state, and
// binds its native session so the next turn resumes it.
func TestARepositoryReadOnlySessionLaunchesItsForkWithAPrivateOutputRoot(t *testing.T) {
	fixture := newSessionACPFixtureOn(t, "normal", "claude@work", true, "")
	argv := recordChildArgv(fixture)
	turn := fixture.submit(t, "inspect this")
	result, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if err != nil || result.State != session.TurnCompleted {
		t.Fatalf("read-only turn = %+v, %v", result, err)
	}
	if want := []string{"fork", "fork", "acp", "claude@work"}; !slices.Equal(*argv, want) {
		t.Fatalf("read-only child argv = %q, want %q", *argv, want)
	}
	if env := readFile(t, fixture.envLog); !strings.Contains(env, "ro=1 authority=") || !strings.Contains(env, "repo="+fixture.repo+" ") {
		t.Fatalf("read-only child environment = %q", env)
	}
	private := filepath.Join(filepath.Dir(filepath.Dir(fixture.private)), "output", fixture.session.ID)
	if info, err := os.Lstat(private); err != nil || !info.IsDir() {
		t.Fatalf("read-only session has no private output root: %v", err)
	}
	if got := sessionNewParams(t, fixture.childLog)["cwd"]; got != StableWorkdir {
		t.Fatalf("read-only session/new cwd = %v, want %s", got, StableWorkdir)
	}
	if text := promptText(t, fixture.childLog); !strings.HasPrefix(text, "inspect this\n\n<coop-output>") {
		t.Fatalf("read-only prompt = %q", text)
	}
	stored, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil || stored.NativeSessionID == "" {
		t.Fatalf("read-only session bound no native session: %+v, %v", stored, err)
	}
}

// A row stored in a removed mode never reaches a child: the launch refuses it before building
// anything, so nothing can run it as a writable session.
func TestASessionStoredInARemovedModeStartsNoChild(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	argv := recordChildArgv(fixture)
	for _, mode := range []string{"readonly", "bare"} {
		bound := fixture.session
		bound.Mode = mode
		_, err := fixture.runner.startChildWithRunID(contextWithTurnDeadline(t), bound, sessionTurnRunID(bound.ID, mode), fixture.private)
		if err == nil || !strings.Contains(err.Error(), "mode or target is invalid") {
			t.Fatalf("a %s session child = %v, want the mode refused", mode, err)
		}
	}
	if len(*argv) != 0 {
		t.Fatalf("a removed-mode session built a child: %q", *argv)
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
		if got := sessionNewParams(t, fixture.childLog)["cwd"]; got != StableWorkdir {
			t.Fatalf("normal session/new cwd = %v, want %s", got, StableWorkdir)
		}
	}
	if workspaces[0] == workspaces[1] {
		t.Fatalf("remote sessions shared a host fork: %q", workspaces[0])
	}
}

// A session uses continuous canonical renewal. A turn need not fit within one
// access token's lifetime, and the child never receives that token.
func TestASessionNeedsOnlyCanonicalStartupReadiness(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal", "claude@work")
	writeSessionTestCredential(t, fixture.source, "claude@work", time.Now().Add(30*time.Minute))
	turn := fixture.submit(t, "horizon")
	result, err := fixture.runner.Run(contextWithTurnTimeout(t, 2*time.Hour), fixture.session, turn)
	if err != nil || result.State != session.TurnCompleted {
		t.Fatalf("turn on a renewable 30-minute token = %+v, %v", result, err)
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
			fixture := newSessionACPFixtureOn(t, "normal", "grok@work", false, network)
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
