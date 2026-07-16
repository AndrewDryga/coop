//go:build providerlivee2e

package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func runProviderResumeLiveCompatibility(
	t *testing.T,
	realConfig *config.Config,
	rt runtime.Runtime,
	runtimeSettings liveprovider.RuntimeSettings,
	target agents.Target,
) liveprovider.ProviderResult {
	t.Helper()
	fail := func(attempted bool, detail string) liveprovider.ProviderResult {
		return liveprovider.ProviderResult{
			Provider: target.Provider, Attempted: attempted, Status: liveprovider.StatusFailed,
			ReasonCode: liveprovider.ReasonHarnessFailed, Phase: "harness",
			ErrorClass: "harness", DetailCode: detail,
		}
	}
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		return fail(false, "layout_setup")
	}
	if err := liveprovider.InitRepository(layout); err != nil {
		return fail(false, "repository_setup")
	}
	before, err := liveprovider.SnapshotRepository(layout)
	if err != nil {
		return fail(false, "repository_snapshot")
	}
	marker, err := liveIdentifier("COOP_LIVE_RESUME_")
	if err != nil {
		return fail(false, "identifier_generation")
	}
	selection, err := liveprovider.SelectionForTarget(realConfig, target)
	if err != nil {
		return fail(false, "target_selection")
	}
	if err := os.Remove(layout.Config); err != nil {
		return fail(false, "config_reset")
	}
	prepared, err := liveprovider.Prepare(realConfig.ConfigDir, layout.Config, []liveprovider.Selection{selection})
	if err != nil {
		result := fail(false, liveprovider.CredentialDetailCode(err))
		result.ReasonCode = liveprovider.ReasonUnsafeCredential
		return result
	}
	defer func() { _ = prepared.Revoke() }()
	preflightReason := prepared.PreflightReason(
		target.Provider, selection.Account, time.Now().Add(30*time.Minute),
	)
	if preflightReason == liveprovider.ReasonUnsafeCredential {
		result := fail(false, "credential_portability")
		result.ReasonCode = liveprovider.ReasonUnsafeCredential
		return result
	}
	if err := prepared.VerifySources(); err != nil {
		return liveprovider.FinalizeResult(
			liveprovider.ProviderResult{Provider: target.Provider},
			liveprovider.VerificationFailures{SourceChanged: true},
		)
	}
	control, _, err := liveprovider.NewProcessControl(layout, false)
	if err != nil {
		return fail(false, "process_control")
	}
	defer control.Close()
	revokePath, err := prepared.RevocationPath()
	if err != nil {
		return fail(false, "credential_revocation")
	}
	ag, ok := agents.Get(target.Provider)
	if !ok {
		return fail(false, "provider_lookup")
	}
	sessionID := ""
	if ag.PresetSessionID() {
		sessionID, err = newSessionID()
		if err != nil {
			return fail(false, "identifier_generation")
		}
	}
	sessionFile := filepath.Join(layout.State, "provider-session-id")

	fresh, freshFailures := runProviderResumeLiveStage(
		layout, rt, runtimeSettings, prepared, control, revokePath, target,
		liveResumeFresh, sessionID, sessionFile, marker, preflightReason,
	)
	freshFailures = verifyProviderResumeLiveStage(layout, before, prepared, freshFailures)
	if !fresh.Passed || freshFailures.CleanupFailed || freshFailures.SourceChanged || freshFailures.RepositoryChanged {
		return finishProviderResumeLive(layout, before, prepared, fresh, freshFailures)
	}
	resolvedID, err := liveprovider.ReadSessionID(layout.Root, sessionFile)
	if err != nil || (sessionID != "" && resolvedID != sessionID) {
		result := fail(true, "session_id")
		result.CLIVersion = fresh.CLIVersion
		return finishProviderResumeLive(layout, before, prepared, result, freshFailures)
	}

	continued, continuedFailures := runProviderResumeLiveStage(
		layout, rt, runtimeSettings, prepared, control, revokePath, target,
		liveResumeContinue, resolvedID, sessionFile, marker, "",
	)
	continued, continuedFailures = carryProviderResumeStage(fresh, freshFailures, continued, continuedFailures)
	continuedFailures = verifyProviderResumeLiveStage(layout, before, prepared, continuedFailures)
	return finishProviderResumeLive(layout, before, prepared, continued, continuedFailures)
}

func carryProviderResumeStage(
	prior liveprovider.ProviderResult,
	priorFailures liveprovider.VerificationFailures,
	current liveprovider.ProviderResult,
	currentFailures liveprovider.VerificationFailures,
) (liveprovider.ProviderResult, liveprovider.VerificationFailures) {
	priorAttempted := prior.Attempted || priorFailures.AttemptedObserved
	currentFailures.AttemptedObserved = currentFailures.AttemptedObserved || priorAttempted
	if current.CLIVersion == "" {
		current.CLIVersion = prior.CLIVersion
	}
	if priorAttempted && !current.Attempted &&
		(current.Status == liveprovider.StatusSkipped ||
			current.ReasonCode == liveprovider.ReasonVersionProbe ||
			current.ReasonCode == liveprovider.ReasonUnsafeCredential) {
		version := current.CLIVersion
		current = providerResumeHarnessFailure(current.Provider, true, "resume_prerequisite")
		current.CLIVersion = version
	}
	return current, currentFailures
}

func runProviderResumeLiveStage(
	layout procharness.Layout,
	rt runtime.Runtime,
	runtimeSettings liveprovider.RuntimeSettings,
	prepared *liveprovider.Prepared,
	control *os.File,
	revokePath string,
	target agents.Target,
	stage, sessionID, sessionFile, marker, preflightReason string,
) (liveprovider.ProviderResult, liveprovider.VerificationFailures) {
	supervisor, err := liveIdentifier("provider-resume-" + stage + "-")
	if err != nil {
		return providerResumeHarnessFailure(target.Provider, false, "identifier_generation"), liveprovider.VerificationFailures{}
	}
	resultFile := filepath.Join(layout.State, "provider-resume-"+stage+"-result.json")
	attemptFile := filepath.Join(layout.State, "provider-resume-"+stage+"-attempted")
	cidDir := filepath.Join(layout.State, "provider-resume-"+stage+"-cids")
	if err := os.Mkdir(cidDir, 0o700); err != nil {
		return providerResumeHarnessFailure(target.Provider, false, "control_directory"), liveprovider.VerificationFailures{}
	}
	env, err := liveprovider.ChildEnvironment(layout, liveprovider.ChildSpec{
		Path: os.Getenv("PATH"), Target: target.String(), Workflow: liveWorkflowResume,
		Stage: stage, SessionID: sessionID, SessionFile: sessionFile, Marker: marker,
		ResultFile: resultFile, AttemptFile: attemptFile, Supervisor: supervisor,
		PreflightReason: preflightReason, CIDDir: cidDir, ControlFD: 3,
		RevokePath: revokePath, Runtime: runtimeSettings,
	})
	if err != nil {
		return providerResumeHarnessFailure(target.Provider, false, "child_environment"), liveprovider.VerificationFailures{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveChildDeadline)
	processResult := procharness.Run(ctx, procharness.Command{
		Path: os.Args[0], Args: []string{"-test.run=^TestProviderLiveChild$", "-test.v=false"},
		Dir: layout.Repo, Env: env, MaxOutput: liveOutputLimit, KillGrace: 3 * time.Second,
		ExtraFiles: []*os.File{control}, BeforeCancel: prepared.Revoke,
	})
	timedOut := liveprovider.ProcessDeadlineExceeded(processResult)
	cancel()
	attempted := liveprovider.ControlFilePresent(layout.Root, attemptFile)
	result := liveprovider.ClassifyChildProcess(target.Provider, layout.Root, resultFile, liveprovider.ChildProcessObservation{
		Result: processResult, DeadlineExceeded: timedOut, Attempted: attempted,
	})
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), liveCleanupDeadline)
	cleanupErr := liveprovider.CleanupSupervisor(cleanupCtx, liveprovider.SupervisorCleanupSpec{
		Root: layout.Root, CIDDir: cidDir, Supervisor: supervisor, LabelKey: box.LabelSupervisor,
		OperationTimeout: 2 * time.Second, QuietPeriod: time.Second, PollInterval: 100 * time.Millisecond,
	}, liveprovider.SupervisorCleanupOps{
		RemoveContainer: rt.RemoveContainerContext,
		RemoveByLabel:   rt.RemoveByLabel,
	})
	cancelCleanup()
	return result, liveprovider.VerificationFailures{
		CleanupFailed: cleanupErr != nil, AttemptedObserved: attempted,
	}
}

func verifyProviderResumeLiveStage(
	layout procharness.Layout,
	before liveprovider.RepositorySnapshot,
	prepared *liveprovider.Prepared,
	failures liveprovider.VerificationFailures,
) liveprovider.VerificationFailures {
	after, err := liveprovider.VerifyRepository(layout, before)
	failures.RepositoryChanged = failures.RepositoryChanged || err != nil || !before.Equal(after)
	failures.SourceChanged = failures.SourceChanged || prepared.VerifySources() != nil
	return failures
}

func finishProviderResumeLive(
	layout procharness.Layout,
	before liveprovider.RepositorySnapshot,
	prepared *liveprovider.Prepared,
	result liveprovider.ProviderResult,
	failures liveprovider.VerificationFailures,
) liveprovider.ProviderResult {
	if err := prepared.Revoke(); err != nil {
		failures.CleanupFailed = true
	}
	failures = verifyProviderResumeLiveStage(layout, before, prepared, failures)
	return liveprovider.FinalizeResult(result, failures)
}

func providerResumeHarnessFailure(provider string, attempted bool, detail string) liveprovider.ProviderResult {
	return liveprovider.ProviderResult{
		Provider: provider, Attempted: attempted, Status: liveprovider.StatusFailed,
		ReasonCode: liveprovider.ReasonHarnessFailed, Phase: "harness",
		ErrorClass: "harness", DetailCode: detail,
	}
}

func providerResumeLiveCommand(ag agents.Agent, cfg *config.Config, stage, sessionID, prompt string) ([]string, bool) {
	if ag.PresetSessionID() {
		var command []string
		if stage == liveResumeFresh {
			if !agents.ValidSessionID(sessionID) {
				return nil, false
			}
			command = ag.StartSession(cfg, sessionID)
		} else {
			var found bool
			command, found = ag.Resume(cfg, box.Workdir(cfg, cfg.RepoOverride), sessionID)
			if !found {
				return nil, false
			}
		}
		return append(command, "-p", prompt), true
	}
	if ag.Stream().Format != agents.StreamCodexJSON {
		return nil, false
	}
	command, streaming := iterationCommand(ag.Name(), ag.Headless(cfg, prompt), nil, true)
	if !streaming || len(command) < 2 || command[1] != "exec" {
		return nil, false
	}
	if stage == liveResumeFresh {
		return command, sessionID == ""
	}
	if !agents.ValidSessionID(sessionID) {
		return nil, false
	}
	resumed := make([]string, 0, len(command)+2)
	resumed = append(resumed, command[:2]...)
	resumed = append(resumed, "resume", sessionID)
	resumed = append(resumed, command[2:]...)
	return resumed, true
}

func providerResumeLiveOutput(ag agents.Agent, expectedID, output string) (sessionID, reply string, err error) {
	if ag.PresetSessionID() {
		if !agents.ValidSessionID(expectedID) {
			return "", "", errors.New("invalid preset session id")
		}
		return expectedID, strings.TrimSpace(output), nil
	}
	if ag.Stream().Format != agents.StreamCodexJSON {
		return "", "", errors.New("unsupported native session stream")
	}
	return parseCodexResumeLiveOutput(expectedID, output)
}

func parseCodexResumeLiveOutput(expectedID, output string) (sessionID, reply string, err error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 64<<10), liveOutputLimit)
	var replies []string
	completed := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			return "", "", errors.New("malformed codex live stream")
		}
		switch event.Type {
		case "thread.started":
			if !agents.ValidSessionID(event.ThreadID) || (sessionID != "" && sessionID != event.ThreadID) {
				return "", "", errors.New("invalid codex live thread")
			}
			sessionID = event.ThreadID
		case "item.completed":
			if event.Item.Type == "agent_message" {
				replies = append(replies, strings.TrimSpace(event.Item.Text))
			}
		case "turn.completed":
			if completed {
				return "", "", errors.New("duplicate codex live completion")
			}
			completed = true
		case "turn.failed":
			return "", "", errors.New("failed codex live stream")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", errors.New("oversized codex live stream")
	}
	if expectedID != "" {
		if !agents.ValidSessionID(expectedID) || sessionID != expectedID {
			return "", "", errors.New("codex resumed the wrong thread")
		}
	}
	if !completed || !agents.ValidSessionID(sessionID) || len(replies) != 1 || replies[0] == "" {
		return "", "", errors.New("incomplete codex live stream")
	}
	return sessionID, replies[0], nil
}

func writeLiveSessionID(path, id string) error {
	if !agents.ValidSessionID(id) {
		return errors.New("invalid live session id")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(id); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func TestProviderResumeLiveContract(t *testing.T) {
	const (
		id     = "018f6352-6281-7ae1-a1d5-07c3399de43d"
		marker = "COOP_LIVE_RESUME_0123456789abcdef0123456789abcdef"
	)
	thread := `{"type":"thread.started","thread_id":"` + id + `"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"` + marker + `"}}` + "\n" +
		`{"type":"turn.completed"}` + "\n"
	gotID, gotReply, err := parseCodexResumeLiveOutput("", thread)
	if err != nil || gotID != id || gotReply != marker {
		t.Fatalf("parse fresh codex stream = %q, %q, %v", gotID, gotReply, err)
	}
	if _, _, err := parseCodexResumeLiveOutput(id, strings.Replace(thread, id, "018f6352-6281-7ae1-b1d5-07c3399de43d", 1)); err == nil {
		t.Fatal("codex stream for a different resumed thread was accepted")
	}
	for _, invalid := range []string{
		`not-json`,
		`{"type":"thread.started","thread_id":"` + id + `"}` + "\n" +
			`{"type":"item.completed","item":{"type":"agent_message","text":"one"}}`,
		`{"type":"thread.started","thread_id":"` + id + `"}` + "\n" +
			`{"type":"item.completed","item":{"type":"agent_message","text":"one"}}` + "\n" +
			`{"type":"turn.failed"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"one"}}` + "\n" +
			`{"type":"item.completed","item":{"type":"agent_message","text":"two"}}` + "\n" +
			`{"type":"turn.completed"}`,
	} {
		if _, _, err := parseCodexResumeLiveOutput(id, invalid); err == nil {
			t.Fatalf("invalid codex stream accepted: %q", invalid)
		}
	}

	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		cfg := &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}
		sessionID := ""
		if ag.PresetSessionID() {
			sessionID = id
		}
		command, ok := providerResumeLiveCommand(ag, cfg, liveResumeFresh, sessionID, marker)
		if !ok || len(command) == 0 {
			t.Errorf("%s has no fresh live resume command", name)
		}
		if ag.PresetSessionID() && (!resumeCommandHasPair(command, "--session-id", id) || !resumeCommandHasPair(command, "-p", marker)) {
			t.Errorf("%s fresh command does not pin the session and prompt: %q", name, command)
		}
		if !ag.PresetSessionID() && (!slices.Contains(command, "--json") || len(command) < 3 || command[1] != "exec") {
			t.Errorf("%s fresh command does not expose its native session stream: %q", name, command)
		}
	}

	for _, name := range agents.Names() {
		t.Run(name+" resume command", func(t *testing.T) {
			ag, _ := agents.Get(name)
			cfg := &config.Config{RepoOverride: t.TempDir(), Workdir: "/workspace/live-repo", ConfigDir: t.TempDir()}
			seedProviderResumeLiveSession(t, cfg, name, id)
			command, ok := providerResumeLiveCommand(ag, cfg, liveResumeContinue, id, "repeat the prior response")
			if !ok {
				t.Fatalf("%s has no exact resume command", name)
			}
			if name == "codex" {
				if len(command) < 4 || command[1] != "exec" || command[2] != "resume" || command[3] != id || !slices.Contains(command, "--json") {
					t.Fatalf("Codex resume did not use its exact native stream: %q", command)
				}
			} else if !resumeCommandHasPair(command, "--resume", id) {
				t.Fatalf("%s resume did not use the exact native id: %q", name, command)
			}
		})
	}

	prior := liveprovider.ProviderResult{Provider: "codex", CLIVersion: "codex-cli 1.2.3", Attempted: true}
	for _, tc := range []struct {
		name       string
		current    liveprovider.ProviderResult
		wantReason string
	}{
		{
			name: "skip after paid fresh request",
			current: liveprovider.ProviderResult{
				Provider: "codex", Status: liveprovider.StatusSkipped, ReasonCode: liveprovider.ReasonMissingCLI,
			},
			wantReason: liveprovider.ReasonHarnessFailed,
		},
		{
			name: "version failure after paid fresh request",
			current: liveprovider.ProviderResult{
				Provider: "codex", Status: liveprovider.StatusFailed, ReasonCode: liveprovider.ReasonVersionProbe,
				Phase: "version", ErrorClass: "process",
			},
			wantReason: liveprovider.ReasonHarnessFailed,
		},
		{
			name: "unsafe credential after paid fresh request",
			current: liveprovider.ProviderResult{
				Provider: "codex", Status: liveprovider.StatusFailed, ReasonCode: liveprovider.ReasonUnsafeCredential,
				Phase: "harness", ErrorClass: "harness", DetailCode: "credential_projection",
			},
			wantReason: liveprovider.ReasonHarnessFailed,
		},
		{
			name:       "harness failure preserves its detail",
			current:    providerResumeHarnessFailure("codex", false, "child_environment"),
			wantReason: liveprovider.ReasonHarnessFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carriedResult, carriedFailures := carryProviderResumeStage(
				prior, liveprovider.VerificationFailures{}, tc.current, liveprovider.VerificationFailures{},
			)
			result := liveprovider.FinalizeResult(carriedResult, carriedFailures)
			if !result.Attempted || result.CLIVersion != prior.CLIVersion || result.ReasonCode != tc.wantReason {
				t.Fatalf("fresh-stage attempt evidence was lost after an early resume outcome: %+v", result)
			}
			if _, err := liveprovider.NewSummary(false, []agents.Target{{Provider: "codex"}}, []liveprovider.ProviderResult{result}); err != nil {
				t.Fatalf("aggregate resume result violates the summary contract: %v (%+v)", err, result)
			}
		})
	}
}

func seedProviderResumeLiveSession(t *testing.T, cfg *config.Config, provider, id string) {
	t.Helper()
	ws := box.Workdir(cfg, cfg.RepoOverride)
	var path, body string
	switch provider {
	case "claude":
		path = filepath.Join(cfg.AgentDir(provider), "projects", agents.ClaudeProjectKey(ws), id+".jsonl")
		body = "{}\n"
	case "codex":
		return
	case "gemini":
		projectHash := fmt.Sprintf("%x", sha256.Sum256([]byte(ws)))
		path = filepath.Join(cfg.AgentDir(provider), "tmp", projectHash, "chats", "session.json")
		body = `{"sessionId":"` + id + `"}`
	case "grok":
		bucket := filepath.Join(cfg.AgentDir(provider), "sessions", "live")
		if err := os.MkdirAll(bucket, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bucket, ".cwd"), []byte(ws+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(bucket, id, "summary.json")
		body = "{}\n"
	default:
		t.Fatalf("unhandled provider %q", provider)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resumeCommandHasPair(command []string, flag, value string) bool {
	for i := 0; i+1 < len(command); i++ {
		if command[i] == flag && command[i+1] == value {
			return true
		}
	}
	return false
}
