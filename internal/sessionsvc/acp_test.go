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

func TestSessionTurnRunnerRotatesToTheNextRungOnARateLimit(t *testing.T) {
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
	// Same provider, so the transcript the limited rung opened still resolves and is reloaded
	// rather than restarted.
	if bound.NativeSessionID != "native-1" {
		t.Fatalf("native session = %q, want the retained native-1", bound.NativeSessionID)
	}
	methods := readSessionACPLog(t, fixture.childLog)
	want := []string{"initialize", "session/new", "session/prompt", "initialize", "session/load", "session/prompt"}
	if fmt.Sprint(methods) != fmt.Sprint(want) {
		t.Fatalf("ACP methods = %v, want %v", methods, want)
	}
	events, err := fixture.store.ListEvents(context.Background(), fixture.session.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	rotated := ""
	for _, event := range events {
		if event.Type == session.EventSessionTargetRotated {
			rotated = string(event.Payload)
		}
	}
	if rotated != `{"from":"codex@work","native_session_reset":false,"to":"codex@backup"}` {
		t.Fatalf("rotation event = %s", rotated)
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
	completed, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
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
	completed, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
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
		"/repo", companions, "/private", "session-000000000000000000000000",
		cfg, "docker",
	) {
		key, value, _ := strings.Cut(item, "=")
		got[key] = value
	}
	for key, want := range map[string]string{
		"COOP_REPO":       "/repo",
		"COOP_CONFIG_DIR": "/private",
		"COOP_HOMES":      "1",
		"COOP_RUNTIME":    "docker",
		"COOP_EGRESS":     "none",
		"COOP_MEMORY":     "2g",
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

func newSessionACPFixture(t *testing.T, scenario string) *sessionACPFixture {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "shared-agents")
	profile := filepath.Join(source, "codex", "profiles", "work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := codexTestCredential(time.Now().Add(2 * time.Hour))
	if err := os.WriteFile(filepath.Join(profile, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"env":             "EMISAR_TOKEN=observe-only\n",
		"mcp.json":        `{"mcpServers":{"emisar":{"url":"https://example.invalid/mcp"}}}`,
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
		Target: "codex@work", Policy: "policy", Repository: repo, Workspace: workspace, ForkName: "fork", BaseCommit: strings.Repeat("a", 40),
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
	return f.submitArtifacts(t, prompt, nil)
}

func (f *sessionACPFixture) submitArtifacts(
	t *testing.T,
	prompt string,
	artifacts []session.InputArtifact,
) session.Turn {
	t.Helper()
	sess, err := f.store.GetSession(context.Background(), f.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.SubmitTurn(context.Background(), "turn-"+prompt, session.SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: prompt, Artifacts: artifacts,
	})
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
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
			switch scenario {
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
