package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
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
	t.Run("refresh required", func(t *testing.T) {
		fixture := newSessionACPFixture(t, "normal")
		credential := filepath.Join(fixture.source, "codex", "profiles", "work", "auth.json")
		if err := os.WriteFile(credential, []byte(codexTestCredential(time.Now().Add(-time.Minute))), 0o600); err != nil {
			t.Fatal(err)
		}
		turn := fixture.submit(t, "expired")
		if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn); err == nil {
			t.Fatal("refresh-required credential unexpectedly projected")
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

func TestSessionTurnRunnerFailsIfServiceCleanupCannotBeProved(t *testing.T) {
	fixture := newSessionACPFixture(t, "normal")
	t.Setenv("COOP_TEST_SESSION_SERVICE_CLEANUP_FAIL", "1")
	turn := fixture.submit(t, "cleanup failure")
	if _, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn); err == nil ||
		!strings.Contains(err.Error(), "session services cleanup failed") {
		t.Fatalf("service cleanup failure = %v", err)
	}
	got, err := fixture.store.GetTurn(context.Background(), fixture.session.ID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != session.TurnFailed || got.ErrorCode != sessionACPCleanupError {
		t.Fatalf("turn after cleanup failure = state %q code %q", got.State, got.ErrorCode)
	}
}

func TestSessionRunIDFromEnv(t *testing.T) {
	t.Setenv("COOP_SESSION_RUN_ID", "not-a-session-run")
	if got := sessionRunIDFromEnv(); got != "" {
		t.Fatalf("invalid session run id = %q", got)
	}
	want := sessionTurnRunID("session", "turn")
	t.Setenv("COOP_SESSION_RUN_ID", want)
	if got := sessionRunIDFromEnv(); got != want {
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
	workspace := forkWorkspace(repo, "fork")
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
