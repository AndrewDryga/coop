//go:build providere2e

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
)

func TestProviderScriptedConsultArms(t *testing.T) {
	suite := newDirectProcessSuite(t)
	providers := agents.Names()
	for index, lead := range providers {
		peer := providers[(index+1)%len(providers)]
		t.Run(lead+"_to_"+peer, func(t *testing.T) {
			baseline, err := liveprovider.SnapshotRepository(suite.layout)
			if err != nil {
				t.Fatal(err)
			}
			prompt := "fixture consult question for " + peer
			reply := "fixture consult reply from " + peer
			model := "env-" + peer
			effort := directDefaultEffort(directProviderContracts[peer])
			scenario := consultProcessScenario(lead, providers,
				[]consultCallSpec{{Target: peer, Mode: "fresh", Prompt: prompt, ExitCode: 0}},
				[]consultStepSpec{{
					Provider: peer, Delivery: "fresh", Result: "usable", Prompt: prompt,
					Reply: reply, Model: model, Effort: effort, UsageIn: consultUsage(peer, 3), UsageOut: consultUsage(peer, 2),
				}},
			)
			result, trace := suite.run(t, []string{lead, "--peer", peer}, scenario)
			if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, reply) {
				t.Fatalf("coop %s --peer %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", lead, peer, result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
			}
			after, err := liveprovider.VerifyRepository(suite.layout, baseline)
			if err != nil {
				t.Fatal(err)
			}
			if !baseline.Equal(after) {
				t.Fatal("consult process changed the writable repository")
			}

			run := oneProcessEvent(t, trace, "runtime", "run")
			assertConsultMounts(t, suite, run, lead, peer)
			assertConsultEnvironment(t, run.Run.Environment, lead, peer, model, effort)
			start := oneProcessEvent(t, trace, "peer", "start")
			if start.Consult == nil {
				t.Fatal("peer start has no consult metadata")
			}
			if start.Consult.Provider != peer || start.Consult.Delivery != "fresh" || start.Consult.Result != "usable" ||
				start.Consult.Model != model || start.Consult.Effort != effort || start.Consult.PromptHash != processTraceValue(prompt) {
				t.Fatalf("peer metadata = %#v", start.Consult)
			}
			wantArgv := consultFreshTraceArgv(peer, model, effort, prompt, start.Consult.SessionHash)
			if !reflect.DeepEqual(start.Argv, wantArgv) {
				t.Fatalf("%s consult argv = %q, want %q", peer, start.Argv, wantArgv)
			}
			assertConsultEnvironment(t, start.Environment, lead, peer, model, effort)
			for _, event := range trace {
				if event.PID > 0 {
					awaitProcessGone(t, event.PID)
				}
			}
		})
	}
}

func TestProviderScriptedConsultDirectedFallbackPairs(t *testing.T) {
	suite := newDirectProcessSuite(t)
	providers := agents.Names()
	pairCount := 0
	for _, first := range providers {
		for _, second := range providers {
			if first == second {
				continue
			}
			pairCount++
			first, second := first, second
			t.Run(first+"_to_"+second, func(t *testing.T) {
				lead := providerPairLead(providers, first, second)
				presetName := "pair-" + first + "-" + second
				persona := suite.writeConsultPairPreset(t, presetName, lead, first, second)
				baseline, err := liveprovider.SnapshotRepository(suite.layout)
				if err != nil {
					t.Fatal(err)
				}
				firstTarget := providerPairTarget(first)
				secondTarget := providerPairTarget(second)
				freshQuestion := "pair fresh question " + first + " " + second
				continueQuestion := "pair follow-up " + first + " " + second
				exhaustQuestion := "pair exhaustion " + first + " " + second
				freshPrompt := consultPersonaPrompt(persona, freshQuestion)
				continuePrompt := consultPersonaPrompt(persona, continueQuestion)
				exhaustPrompt := consultPersonaPrompt(persona, exhaustQuestion)
				reply := "pair fallback reply from " + second
				continuedReply := "pair continued reply from " + second
				calls := []consultCallSpec{
					{Target: "advisor", Mode: "fresh", Prompt: freshQuestion, ExitCode: 0},
					{Target: "advisor", Mode: "continue", Prompt: continueQuestion, ExitCode: 0},
					{Target: "advisor", Mode: "fresh", Prompt: exhaustQuestion, ExitCode: 75},
				}
				steps := []consultStepSpec{
					consultPairStep(firstTarget, "fresh", "limited", freshPrompt, ""),
					consultPairStep(secondTarget, "fresh", "usable", freshPrompt, reply),
					consultPairStep(secondTarget, "resume", "usable", continuePrompt, continuedReply),
					consultPairStep(firstTarget, "fresh", "limited", exhaustPrompt, ""),
					consultPairStep(secondTarget, "fresh", "limited", exhaustPrompt, ""),
				}
				result, trace := suite.run(t, []string{presetName}, consultProcessScenario(lead, providers, calls, steps))
				if result.Err != nil || result.ExitCode != 75 || !strings.Contains(result.Stdout, reply) || !strings.Contains(result.Stdout, continuedReply) ||
					!strings.Contains(result.Stderr, "exhausted all 2 targets") {
					t.Fatalf("pair %s -> %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", first, second, result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
				}
				after, err := liveprovider.VerifyRepository(suite.layout, baseline)
				if err != nil {
					t.Fatal(err)
				}
				if !baseline.Equal(after) {
					t.Fatal("directed fallback pair changed the writable repository")
				}
				starts := processEvents(trace, "peer", "start")
				if len(starts) != len(steps) {
					t.Fatalf("pair started %d peer attempts, want %d", len(starts), len(steps))
				}
				for index, event := range starts {
					want := steps[index]
					if event.Consult == nil || event.Consult.Step != index || event.Consult.Provider != want.Provider ||
						event.Consult.Delivery != want.Delivery || event.Consult.Result != want.Result ||
						event.Consult.PromptHash != processTraceValue(want.Prompt) || event.Consult.Model != want.Model || event.Consult.Effort != want.Effort {
						t.Errorf("pair peer step %d = %#v, want %#v", index, event.Consult, want)
					}
				}
				run := oneProcessEvent(t, trace, "runtime", "run")
				assertConsultRoleWiring(t, run.Run, lead, firstTarget, secondTarget)
				for _, event := range trace {
					if event.PID > 0 {
						awaitProcessGone(t, event.PID)
					}
				}
			})
		}
	}
	if pairCount != len(providers)*(len(providers)-1) {
		t.Fatalf("directed fallback pairs = %d, providers = %v", pairCount, providers)
	}
}

func TestProviderScriptedConsultContinuityRecoveryMatrix(t *testing.T) {
	suite := newDirectProcessSuite(t)
	providers := agents.Names()
	for index, peer := range providers {
		lead := providers[(index+1)%len(providers)]
		peer, lead := peer, lead
		t.Run(peer, func(t *testing.T) {
			t.Run("deleted id replays bounded transcript", func(t *testing.T) {
				q1, r1 := "continuity first question", "continuity first reply"
				q2, r2 := "continuity delta", "successful prose may mention rate limit without falling back"
				q3, r3 := "continuity after local id deletion", "continuity recovered reply"
				calls := []consultCallSpec{
					{Target: peer, Mode: "fresh", Prompt: q1, ExitCode: 0},
					{Target: peer, Mode: "continue", Prompt: q2, ExitCode: 0},
					{Target: peer, Mode: "continue", Prompt: q3, ExitCode: 0, DropID: true},
				}
				steps := []consultStepSpec{
					consultDirectStep(peer, "fresh", "usable", q1, r1),
					consultDirectStep(peer, "resume", "usable", q2, r2),
					consultDirectStep(peer, "fresh", "usable", consultRecoveryPrompt([]consultTurn{{q1, r1}, {q2, r2}}, q3), r3),
				}
				result, trace := suite.run(t, []string{lead, "--peer", peer}, consultProcessScenario(lead, providers, calls, steps))
				if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, r3) || !strings.Contains(result.Stdout, "started FRESH from the saved transcript") {
					t.Fatalf("deleted-id recovery for %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s", peer, result.ExitCode, result.Err, result.Stdout, result.Stderr)
				}
				assertConsultDeliverySequence(t, trace, []string{"fresh", "resume", "fresh"})
				assertConsultSessionSequence(t, trace)
				assertConsultStateSecure(t, suite, peer, true)
			})

			t.Run("failed resume recovers next call", func(t *testing.T) {
				q1, r1 := "failed-resume first question", "failed-resume first reply"
				q2 := "failed-resume delta"
				q3, r3 := "failed-resume retry", "failed-resume recovered reply"
				calls := []consultCallSpec{
					{Target: peer, Mode: "fresh", Prompt: q1, ExitCode: 0},
					{Target: peer, Mode: "continue", Prompt: q2, ExitCode: 42},
					{Target: peer, Mode: "continue", Prompt: q3, ExitCode: 0},
				}
				steps := []consultStepSpec{
					consultDirectStep(peer, "fresh", "usable", q1, r1),
					consultDirectStep(peer, "resume", "failed-resume", q2, ""),
					consultDirectStep(peer, "fresh", "usable", consultRecoveryPrompt([]consultTurn{{q1, r1}}, q3), r3),
				}
				result, trace := suite.run(t, []string{lead, "--peer", peer}, consultProcessScenario(lead, providers, calls, steps))
				if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, r3) {
					t.Fatalf("failed-resume recovery for %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", peer, result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
				}
				assertConsultDeliverySequence(t, trace, []string{"fresh", "resume", "fresh"})
				assertConsultStateSecure(t, suite, peer, true)
			})
		})
	}
}

func TestProviderScriptedConsultFailureMatrix(t *testing.T) {
	suite := newDirectProcessSuite(t)
	providers := agents.Names()
	for index, peer := range providers {
		lead := providers[(index+1)%len(providers)]
		peer, lead := peer, lead
		t.Run(peer, func(t *testing.T) {
			calls := []consultCallSpec{
				{Target: peer, Mode: "fresh", Prompt: "empty question", ExitCode: 1},
				{Target: peer, Mode: "fresh", Prompt: "stderr-only question", ExitCode: 1},
			}
			steps := []consultStepSpec{
				consultDirectStep(peer, "fresh", "empty", "empty question", ""),
				consultDirectDiagnosticStep(peer, "stderr-only", "stderr-only question", "fixture stderr diagnostic"),
			}
			if peer == "codex" {
				calls = append(calls, consultCallSpec{Target: peer, Mode: "fresh", Prompt: "malformed question", ExitCode: 1})
				steps = append(steps, consultDirectStep(peer, "fresh", "malformed", "malformed question", ""))
			}
			calls = append(calls, consultCallSpec{Target: peer, Mode: "fresh", Prompt: "ordinary failure question", ExitCode: 23})
			steps = append(steps, consultDirectDiagnosticStep(peer, "ordinary", "ordinary failure question", "fixture ordinary diagnostic"))
			result, trace := suite.run(t, []string{lead, "--peer", peer}, consultProcessScenario(lead, providers, calls, steps))
			if result.Err != nil || result.ExitCode != 23 || strings.Contains(result.Stdout, "fixture stderr diagnostic") || !strings.Contains(result.Stderr, "fixture stderr diagnostic") {
				t.Fatalf("failure matrix for %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s", peer, result.ExitCode, result.Err, result.Stdout, result.Stderr)
			}
			if got := len(processEvents(trace, "peer", "start")); got != len(steps) {
				t.Fatalf("failure matrix for %s started %d peers, want %d", peer, got, len(steps))
			}
			assertConsultStateSecure(t, suite, peer, false)
		})
	}
}

func TestProviderScriptedConsultTimeoutAndOverflowMatrix(t *testing.T) {
	suite := newDirectProcessSuite(t)
	providers := agents.Names()
	assertProcessesGone := func(t *testing.T, trace []*processTrace) {
		t.Helper()
		for _, event := range trace {
			if event.PID > 0 {
				awaitProcessGone(t, event.PID)
			}
		}
	}
	setTimeout := func(seconds int) {
		t.Helper()
		path := filepath.Join(suite.layout.Config, "env")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines, found := strings.Split(string(body), "\n"), 0
		for index, line := range lines {
			if strings.HasPrefix(line, "COOP_CONSULT_TIMEOUT=") {
				lines[index] = fmt.Sprintf("COOP_CONSULT_TIMEOUT=%d", seconds)
				found++
			}
		}
		if found != 1 {
			t.Fatalf("fixture env has %d COOP_CONSULT_TIMEOUT entries, want 1", found)
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("timeout", func(t *testing.T) {
		for index, peer := range providers {
			lead := providers[(index+1)%len(providers)]
			peer, lead := peer, lead
			t.Run(peer, func(t *testing.T) {
				calls := []consultCallSpec{{Target: peer, Mode: "fresh", Prompt: "timeout question", ExitCode: 124}}
				steps := []consultStepSpec{consultDirectStep(peer, "fresh", "timeout", "timeout question", "")}
				result, trace := suite.run(t, []string{lead, "--peer", peer}, consultProcessScenario(lead, providers, calls, steps))
				if result.Err != nil || result.ExitCode != 124 || !strings.Contains(result.Stderr, "no reply within 2s") {
					t.Fatalf("timeout for %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s", peer, result.ExitCode, result.Err, result.Stdout, result.Stderr)
				}
				if len(processEvents(trace, "peer", "ready")) != 1 || len(processEvents(trace, "timeout", "exit")) != 1 {
					t.Fatalf("timeout trace for %s did not own the provider process:\n%s", peer, readProcessFile(t, suite.layout.Trace))
				}
				assertConsultStateSecure(t, suite, peer, false)
				assertProcessesGone(t, trace)
			})
		}
	})

	// Overflow classification must not race the deliberately tiny deadline used above.
	setTimeout(10)
	t.Run("overflow", func(t *testing.T) {
		for index, peer := range providers {
			lead := providers[(index+1)%len(providers)]
			peer, lead := peer, lead
			t.Run(peer, func(t *testing.T) {
				calls := []consultCallSpec{
					{Target: peer, Mode: "fresh", Prompt: "overflow question", ExitCode: 1},
					{Target: peer, Mode: "fresh", Prompt: "diagnostic overflow question", ExitCode: 1},
				}
				steps := []consultStepSpec{
					consultDirectStep(peer, "fresh", "overflow", "overflow question", ""),
					consultDirectStep(peer, "fresh", "diagnostic-overflow", "diagnostic overflow question", ""),
				}
				result, trace := suite.run(t, []string{lead, "--peer", peer}, consultProcessScenario(lead, providers, calls, steps))
				replyOverflow := strings.Contains(result.Stderr, "reply exceeded") || (peer == "codex" && strings.Contains(result.Stderr, "Codex output exceeded"))
				if result.Err != nil || result.ExitCode != 1 || len(result.Stdout) > 64<<10 ||
					!replyOverflow || !strings.Contains(result.Stderr, "diagnostics exceeded") {
					t.Fatalf("overflow for %s = exit %d err %v stdout-bytes %d\nstdout:\n%s\nstderr:\n%s", peer, result.ExitCode, result.Err, len(result.Stdout), result.Stdout, result.Stderr)
				}
				run := oneProcessEvent(t, trace, "runtime", "run")
				if run.Run == nil || processEnvironmentValue(run.Run.Environment, "COOP_CONSULT_TIMEOUT") != "10" {
					t.Fatalf("overflow runtime did not receive COOP_CONSULT_TIMEOUT=10: %#v", run.Run)
				}
				if strings.Contains(result.Stderr, strings.Repeat("d", 1024)) {
					t.Fatalf("diagnostic overflow leaked a partial provider stream for %s", peer)
				}
				if len(processEvents(trace, "peer", "ready")) != 0 || len(processEvents(trace, "timeout", "exit")) != 2 {
					t.Fatalf("overflow trace for %s did not own both provider processes:\n%s", peer, readProcessFile(t, suite.layout.Trace))
				}
				assertConsultStateSecure(t, suite, peer, false)
				assertProcessesGone(t, trace)
			})
		}
	})
}

func TestProviderScriptedConsultPromptAndTranscriptBounds(t *testing.T) {
	suite := newDirectProcessSuite(t)
	providers := agents.Names()
	for index, peer := range providers {
		lead := providers[(index+1)%len(providers)]
		peer, lead := peer, lead
		t.Run(peer, func(t *testing.T) {
			calls := []consultCallSpec{
				{Target: peer, Mode: "fresh", PromptKind: "oversized-input", ExitCode: 2},
				{Target: peer, Mode: "fresh", Prompt: "first bounded turn", ExitCode: 0},
				{Target: peer, Mode: "continue", Prompt: "second bounded turn", ExitCode: 0},
			}
			steps := []consultStepSpec{
				consultDirectStep(peer, "fresh", "large-reply", "first bounded turn", ""),
				consultDirectStep(peer, "resume", "large-reply", "second bounded turn", ""),
			}
			result, trace := suite.run(t, []string{lead, "--peer", peer}, consultProcessScenario(lead, providers, calls, steps))
			if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stderr, "question exceeds 524288 bytes") ||
				!strings.Contains(result.Stderr, "continuity exceeded 524288 bytes") {
				t.Fatalf("prompt/transcript bounds for %s = exit %d err %v\nstdout-bytes: %d\nstderr:\n%s", peer, result.ExitCode, result.Err, len(result.Stdout), result.Stderr)
			}
			if got := len(processEvents(trace, "peer", "start")); got != 2 {
				t.Fatalf("oversized input dispatched a provider for %s: %d peer calls", peer, got)
			}
			assertConsultStateSecure(t, suite, peer, false)
		})
	}
}

func TestProviderScriptedConsultConstructedPromptBound(t *testing.T) {
	suite := newDirectProcessSuite(t)
	lead, peer, presetName := "claude", "codex", "constructed-bound"
	dir := filepath.Join(suite.layout.Repo, ".agent", "presets", presetName)
	if err := os.MkdirAll(filepath.Join(dir, "roles"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte("lead: {agent: "+lead+"}\nroles:\n  advisor:\n    mode: consult\n    agent: "+peer+"\n    prompt: roles/advisor.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roles", "advisor.md"), []byte(strings.Repeat("p", 500<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	question := strings.Repeat("q", 20<<10)
	scenario := consultProcessScenario(lead, suite.providers,
		[]consultCallSpec{{Target: "advisor", Mode: "fresh", Prompt: question, ExitCode: 2}}, nil)
	result, trace := suite.run(t, []string{presetName}, scenario)
	if result.ExitCode != 2 || !strings.Contains(result.Stderr, "constructed prompt exceeds 524288 bytes") {
		t.Fatalf("constructed prompt bound = exit %d err %v\n%s", result.ExitCode, result.Err, result.Stderr)
	}
	if got := len(processEvents(trace, "peer", "start")); got != 0 {
		t.Fatalf("constructed prompt overflow dispatched %d providers", got)
	}
}

func TestProviderScriptedConsultLoopTelemetry(t *testing.T) {
	suite := newDirectProcessSuite(t)
	lead := providerPairLead(suite.providers, "codex", "gemini")
	presetName := "consult-telemetry"
	persona := suite.writeConsultPairPreset(t, presetName, lead, "codex", "gemini")
	loopConfig := filepath.Join(suite.layout.Repo, ".agent", "loop.yaml")
	if err := os.WriteFile(loopConfig, []byte("work:\n  command: [fixture-consult]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	codexTarget := providerPairTarget("codex")

	cases := []struct {
		name    string
		calls   []consultCallSpec
		steps   []consultStepSpec
		wantRow bool
	}{
		{
			name: "valid codex usage",
			calls: []consultCallSpec{
				{Target: "advisor", Mode: "fresh", Prompt: "usage question", ExitCode: 0},
			},
			steps: []consultStepSpec{
				consultPairStep(codexTarget, "fresh", "usable", consultPersonaPrompt(persona, "usage question"), "usage reply"),
			},
			wantRow: true,
		},
		{
			name: "codex without usage event",
			calls: []consultCallSpec{
				{Target: "advisor", Mode: "fresh", Prompt: "no usage question", ExitCode: 0},
			},
			steps: []consultStepSpec{
				{
					Provider: "codex", Delivery: "fresh", Result: "usable",
					Prompt: consultPersonaPrompt(persona, "no usage question"), Reply: "no usage reply",
					Model: codexTarget.Model, Effort: codexTarget.Effort,
				},
			},
		},
		{
			name: "failed codex and plain provider",
			calls: []consultCallSpec{
				{Target: "advisor", Mode: "fresh", Prompt: "failed usage question", ExitCode: 23},
				{Target: "gemini", Mode: "fresh", Prompt: "plain usage question", ExitCode: 0},
			},
			steps: []consultStepSpec{
				consultPairStep(codexTarget, "fresh", "ordinary", consultPersonaPrompt(persona, "failed usage question"), ""),
				consultDirectStep("gemini", "fresh", "usable", "plain usage question", "plain reply"),
			},
		},
	}

	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			taskID := fmt.Sprintf("2026-07-15-consult-telemetry-%d", index)
			prepareConsultLoopTask(t, suite.layout.Repo, taskID)
			defer cleanupConsultLoopCase(suite.layout.Repo, taskID)
			baseline, err := liveprovider.SnapshotRepository(suite.layout)
			if err != nil {
				t.Fatal(err)
			}
			scenario := consultProcessScenario(lead, suite.providers, tc.calls, tc.steps)
			scenario.Consult.BlockTask = taskID
			result, trace := suite.run(t, []string{"loop", presetName, "--max-tasks", "1", "--no-preflight"}, scenario)
			if result.Err != nil || result.ExitCode != 0 {
				t.Fatalf("telemetry loop = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
			}
			run := oneProcessEvent(t, trace, "runtime", "run")
			runID := processEnvironmentValue(run.Run.Environment, "COOP_RUN_ID")
			if !lowerHexRunID(runID) {
				t.Fatalf("loop COOP_RUN_ID = %q, want 16 lowercase hex characters", runID)
			}
			blocked := filepath.Join(suite.layout.Repo, ".agent", "tasks", "50_blocked", taskID)
			if info, err := os.Lstat(blocked); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("fixture did not settle exactly the claimed task: %v", err)
			}
			peerPath := filepath.Join(suite.layout.Repo, ".agent", "runs", runID+".peers.jsonl")
			rows := readPeerRecords(suite.layout.Repo, runID)
			if tc.wantRow {
				info, err := os.Lstat(peerPath)
				if err != nil {
					t.Fatal(err)
				}
				stat, ok := info.Sys().(*syscall.Stat_t)
				if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || stat.Nlink != 1 {
					t.Fatalf("peer telemetry target = mode %s stat %#v", info.Mode(), stat)
				}
				if len(rows) != 1 || rows[0] != (peerRecord{Run: runID, Role: "advisor", Provider: "codex", Model: codexTarget.Model, In: 7, Out: 3}) {
					t.Fatalf("peer telemetry rows = %#v", rows)
				}
				data, err := os.ReadFile(peerPath)
				if err != nil {
					t.Fatal(err)
				}
				if len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 1 {
					t.Fatalf("peer telemetry is not exactly one JSONL row: %q", data)
				}
				var decoded peerRecord
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatalf("peer telemetry is not valid JSON: %v", err)
				}
			} else {
				if len(rows) != 0 {
					t.Fatalf("no-usage case wrote peer telemetry: %#v", rows)
				}
				if _, err := os.Lstat(peerPath); !os.IsNotExist(err) {
					t.Fatalf("empty peer telemetry file was not removed: %v", err)
				}
			}
			restoreConsultLoopTask(t, suite.layout.Repo, taskID)
			if err := os.RemoveAll(filepath.Join(suite.layout.Repo, ".agent", "runs")); err != nil {
				t.Fatal(err)
			}
			after, err := liveprovider.VerifyRepository(suite.layout, baseline)
			if err != nil || !baseline.Equal(after) {
				t.Fatalf("telemetry loop changed repository outside its validated ignored row: equal=%v err=%v", baseline.Equal(after), err)
			}
			if err := os.RemoveAll(filepath.Join(suite.layout.Repo, ".agent", "tasks", "00_todo", taskID)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProviderScriptedConsultScopeFailures(t *testing.T) {
	suite := newDirectProcessSuite(t)

	t.Run("unauthenticated ad hoc peer fails before runtime", func(t *testing.T) {
		disableProcessCredential(t, suite, "gemini")
		result, trace := suite.run(t, []string{"claude", "--peer", "gemini"}, processScenario("claude", nil, 0, ""))
		if result.Err != nil || result.ExitCode != 2 || len(trace) != 0 || !strings.Contains(result.Stderr, `--peer "gemini" isn't signed in`) {
			t.Fatalf("unauthenticated peer rejection = exit %d err %v trace %d\nstderr:\n%s", result.ExitCode, result.Err, len(trace), result.Stderr)
		}
	})

	t.Run("malformed role ladder fails before runtime", func(t *testing.T) {
		name := "invalid-consult-ladder"
		dir := filepath.Join(suite.layout.Repo, ".agent", "presets", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "lead: {agent: claude}\nroles:\n  advisor:\n    mode: consult\n    agent: [codex, not-a-provider]\n"
		if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		result, trace := suite.run(t, []string{name}, processScenario("claude", nil, 0, ""))
		if result.Err != nil || result.ExitCode != 2 || len(trace) != 0 || !strings.Contains(result.Stderr, "unknown provider") {
			t.Fatalf("invalid role ladder rejection = exit %d err %v trace %d\nstderr:\n%s", result.ExitCode, result.Err, len(trace), result.Stderr)
		}
	})

	t.Run("unlisted direct peer fails before provider dispatch", func(t *testing.T) {
		baseline, err := liveprovider.SnapshotRepository(suite.layout)
		if err != nil {
			t.Fatal(err)
		}
		scenario := consultProcessScenario("claude", suite.providers,
			[]consultCallSpec{{Target: "gemini", Mode: "fresh", Prompt: "out of scope", ExitCode: 2}}, nil)
		result, trace := suite.run(t, []string{"claude", "--peer", "codex"}, scenario)
		if result.Err != nil || result.ExitCode != 2 || !strings.Contains(result.Stderr, "not in this run's credential scope") {
			t.Fatalf("unlisted peer rejection = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if got := len(processEvents(trace, "peer", "start")); got != 0 {
			t.Fatalf("unlisted peer started %d provider processes", got)
		}
		after, err := liveprovider.VerifyRepository(suite.layout, baseline)
		if err != nil || !baseline.Equal(after) {
			t.Fatalf("unlisted peer rejection changed repository: equal=%v err=%v", baseline.Equal(after), err)
		}
	})

	t.Run("unmounted fallback is skipped before available dispatch", func(t *testing.T) {
		disableProcessCredential(t, suite, "gemini")
		name := "missing-fallback"
		persona := suite.writeConsultPairPreset(t, name, "claude", "codex", "gemini")
		target := providerPairTarget("codex")
		question := "available rung question"
		scenario := consultProcessScenario("claude", suite.providers,
			[]consultCallSpec{{Target: "advisor", Mode: "fresh", Prompt: question, ExitCode: 0}},
			[]consultStepSpec{consultPairStep(target, "fresh", "usable", consultPersonaPrompt(persona, question), "available rung reply")})
		result, trace := suite.run(t, []string{name}, scenario)
		if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stderr, "skipping gemini") {
			t.Fatalf("missing fallback handling = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		starts := processEvents(trace, "peer", "start")
		if len(starts) != 1 || starts[0].Consult == nil || starts[0].Consult.Provider != "codex" {
			t.Fatalf("missing fallback peer starts = %#v", starts)
		}
		run := oneProcessEvent(t, trace, "runtime", "run")
		missingSource := processTracePath(suite.layout.Root, filepath.Join(suite.layout.Config, "gemini", "profiles", "personal"))
		for _, mount := range run.Run.Mounts {
			if mount.Source == missingSource {
				t.Fatalf("unauthenticated fallback credential was mounted: %#v", mount)
			}
		}
	})

	t.Run("all unmounted role targets fail before provider dispatch", func(t *testing.T) {
		disableProcessCredential(t, suite, "gemini")
		name := "all-targets-missing"
		writeSingleConsultPreset(t, suite.layout.Repo, name, "claude", "gemini")
		scenario := consultProcessScenario("claude", suite.providers,
			[]consultCallSpec{{Target: "advisor", Mode: "fresh", Prompt: "no mounted target", ExitCode: 2}}, nil)
		result, trace := suite.run(t, []string{name}, scenario)
		if result.Err != nil || result.ExitCode != 2 || !strings.Contains(result.Stderr, "no target with mounted credentials") {
			t.Fatalf("all-unmounted role rejection = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if got := len(processEvents(trace, "peer", "start")); got != 0 {
			t.Fatalf("all-unmounted role started %d provider processes", got)
		}
	})
}

func disableProcessCredential(t *testing.T, suite *directProcessSuite, provider string) {
	t.Helper()
	ag, ok := agents.Get(provider)
	if !ok {
		t.Fatalf("provider %q is not registered", provider)
	}
	marker, _ := ag.AuthMarker()
	markerPath := filepath.Join(suite.layout.Config, provider, "profiles", "personal", marker)
	markerBody, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(suite.layout.Config, "env")
	envBody, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	drop := map[string]bool{}
	for _, key := range ag.CredentialEnvKeys() {
		drop[key] = true
	}
	var filtered []string
	for _, line := range strings.Split(string(envBody), "\n") {
		key, _, _ := strings.Cut(line, "=")
		if !drop[key] {
			filtered = append(filtered, line)
		}
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(strings.Join(filtered, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(markerPath, markerBody, 0o600)
		_ = os.WriteFile(envPath, envBody, 0o600)
	})
}

func writeSingleConsultPreset(t *testing.T, repo, name, lead, peer string) {
	t.Helper()
	dir := filepath.Join(repo, ".agent", "presets", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("lead: {agent: %s}\nroles:\n  advisor:\n    mode: consult\n    agent: %s\n", lead, peer)
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func prepareConsultLoopTask(t *testing.T, repo, id string) {
	t.Helper()
	root := filepath.Join(repo, ".agent", "tasks")
	for _, state := range []string{"00_todo", "10_in_progress", "50_blocked", "99_done"} {
		if err := os.MkdirAll(filepath.Join(root, state), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	task := filepath.Join(root, "00_todo", id)
	if err := os.Mkdir(task, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task, "task.md"), []byte("# Deterministic consult telemetry fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func restoreConsultLoopTask(t *testing.T, repo, id string) {
	t.Helper()
	root := filepath.Join(repo, ".agent", "tasks")
	blocked := filepath.Join(root, "50_blocked", id)
	if err := os.Remove(filepath.Join(blocked, "decision.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(blocked, filepath.Join(root, "00_todo", id)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "00_todo", id, "tmp")); err != nil {
		t.Fatal(err)
	}
}

func cleanupConsultLoopCase(repo, id string) {
	root := filepath.Join(repo, ".agent", "tasks")
	todo := filepath.Join(root, "00_todo", id)
	for _, state := range []string{"10_in_progress", "50_blocked", "99_done"} {
		path := filepath.Join(root, state, id)
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		_ = os.Remove(filepath.Join(path, "decision.md"))
		_ = os.RemoveAll(filepath.Join(path, "tmp"))
		if _, err := os.Lstat(todo); os.IsNotExist(err) {
			_ = os.Rename(path, todo)
		}
	}
	_ = os.RemoveAll(filepath.Join(repo, ".agent", "runs"))
	_ = os.RemoveAll(todo)
}

func processEnvironmentValue(environment []processEnv, key string) string {
	for _, entry := range environment {
		if entry.Name == key {
			return entry.Value
		}
	}
	return ""
}

func lowerHexRunID(value string) bool {
	if len(value) != 16 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func providerPairLead(providers []string, first, second string) string {
	for _, provider := range providers {
		if provider != first && provider != second {
			return provider
		}
	}
	panic("provider pair has no distinct lead")
}

type consultTurn struct {
	question string
	reply    string
}

type consultCallSpec struct {
	Target     string `json:"target"`
	Mode       string `json:"mode"`
	Prompt     string `json:"prompt,omitempty"`
	PromptKind string `json:"prompt_kind,omitempty"`
	ExitCode   int    `json:"exit_code"`
	DropID     bool   `json:"drop_id,omitempty"`
}

type consultStepSpec struct {
	Provider    string `json:"provider"`
	Delivery    string `json:"delivery"`
	Result      string `json:"result"`
	Prompt      string `json:"prompt"`
	Reply       string `json:"reply,omitempty"`
	Diagnostics string `json:"diagnostics,omitempty"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	ExitCode    int    `json:"exit_code,omitempty"`
	UsageIn     int    `json:"usage_in,omitempty"`
	UsageOut    int    `json:"usage_out,omitempty"`
}

type consultProcessPlan struct {
	Calls     []consultCallSpec `json:"calls"`
	Steps     []consultStepSpec `json:"steps"`
	BlockTask string            `json:"block_task,omitempty"`
}

type consultProcessSpec struct {
	Version       int                `json:"version"`
	Provider      string             `json:"provider"`
	ProviderHomes []string           `json:"provider_homes"`
	Consult       consultProcessPlan `json:"consult"`
}

func consultRecoveryPrompt(turns []consultTurn, current string) string {
	var transcript strings.Builder
	for index, turn := range turns {
		if index > 0 {
			transcript.WriteString("\n\n---\n\n")
		}
		fmt.Fprintf(&transcript, "Question:\n%s\n\nReply:\n%s\n", turn.question, turn.reply)
	}
	return "Continue this consult from the saved transcript:\n\n" + transcript.String() + "\n\nCurrent follow-up:\n" + current
}

func consultDirectStep(provider, delivery, result, prompt, reply string) consultStepSpec {
	step := consultStepSpec{
		Provider: provider, Delivery: delivery, Result: result, Prompt: prompt,
		Model: "env-" + provider, Effort: directDefaultEffort(directProviderContracts[provider]),
	}
	step.Reply = reply
	if provider == "codex" && result == "usable" {
		step.UsageIn, step.UsageOut = 5, 2
	}
	return step
}

func consultDirectDiagnosticStep(provider, result, prompt, diagnostics string) consultStepSpec {
	step := consultDirectStep(provider, "fresh", result, prompt, "")
	step.Diagnostics = diagnostics
	return step
}

func assertConsultDeliverySequence(t *testing.T, trace []*processTrace, want []string) {
	t.Helper()
	starts := processEvents(trace, "peer", "start")
	if len(starts) != len(want) {
		t.Fatalf("peer deliveries = %d events, want %d", len(starts), len(want))
	}
	for index, delivery := range want {
		if starts[index].Consult == nil || starts[index].Consult.Delivery != delivery {
			t.Errorf("peer delivery %d = %#v, want %q", index, starts[index].Consult, delivery)
		}
	}
}

func assertConsultSessionSequence(t *testing.T, trace []*processTrace) {
	t.Helper()
	starts := processEvents(trace, "peer", "start")
	if len(starts) != 3 || starts[0].Consult == nil || starts[1].Consult == nil || starts[2].Consult == nil {
		t.Fatalf("session sequence is incomplete: %#v", starts)
	}
	first, resumed, recovered := starts[0].Consult.SessionHash, starts[1].Consult.SessionHash, starts[2].Consult.SessionHash
	if first == "" || resumed != first || recovered == "" || recovered == first {
		t.Errorf("session hashes = fresh %q resume %q recovered %q", first, resumed, recovered)
	}
}

func assertConsultStateSecure(t *testing.T, suite *directProcessSuite, target string, present bool) {
	t.Helper()
	key := strings.ToUpper(strings.ReplaceAll(target, "-", "_"))
	dir := filepath.Join(suite.layout.Tmp, "coop-consult-state")
	path := filepath.Join(dir, key+".state")
	info, err := os.Lstat(path)
	if !present {
		if !os.IsNotExist(err) {
			t.Errorf("terminal consult retained continuation state: %v", err)
		}
	} else if err != nil {
		t.Errorf("successful consult is missing continuation state: %v", err)
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Errorf("consult continuation state mode = %s", info.Mode())
	} else if data, readErr := os.ReadFile(path); readErr != nil || !bytes.HasPrefix(data, []byte("COOP-CONSULT-STATE 1\n")) {
		t.Errorf("consult continuation record is invalid: %v", readErr)
	}
	lockPath := filepath.Join(dir, key+".lock")
	if lockInfo, lockErr := os.Lstat(lockPath); lockErr == nil {
		stat, ok := lockInfo.Sys().(*syscall.Stat_t)
		if !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 || !ok || stat.Nlink != 1 {
			t.Errorf("consult flock inode is unsafe: mode=%s stat=%#v", lockInfo.Mode(), stat)
		} else if lock, err := os.OpenFile(lockPath, os.O_RDWR, 0); err != nil {
			t.Errorf("open released consult lock: %v", err)
		} else {
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Errorf("consult kernel lock survived wrapper exit: %v", err)
			} else if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
				t.Errorf("release verification lock: %v", err)
			}
			_ = lock.Close()
		}
	} else if !os.IsNotExist(lockErr) {
		t.Errorf("inspect consult lock: %v", lockErr)
	}
	if _, err := os.Lstat(lockPath + ".d"); !os.IsNotExist(err) {
		t.Errorf("consult fallback lock survived wrapper exit: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(suite.layout.Tmp, "coop-consult-*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		if filepath.Base(match) != "coop-consult-state" {
			t.Errorf("consult attempt state survived wrapper exit: %s", match)
		}
	}
}

func providerPairTarget(provider string) agents.Target {
	model := "fixture-" + provider + "-model"
	raw := provider + ":" + model
	if directProviderContracts[provider].supportsEffort {
		raw += "/high"
	}
	target, err := agents.ParseTarget(raw)
	if err != nil {
		panic(err)
	}
	return target
}

func (s *directProcessSuite) writeConsultPairPreset(t *testing.T, name, lead, first, second string) string {
	t.Helper()
	dir := filepath.Join(s.layout.Repo, ".agent", "presets", name)
	roles := filepath.Join(dir, "roles")
	if err := os.MkdirAll(roles, 0o700); err != nil {
		t.Fatal(err)
	}
	firstTarget, secondTarget := providerPairTarget(first), providerPairTarget(second)
	body := fmt.Sprintf("lead: {agent: %s}\nroles:\n  advisor:\n    mode: consult\n    agent: [%s, %s]\n    prompt: roles/advisor.md\n", lead, firstTarget.String(), secondTarget.String())
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	persona := "You are the deterministic read-only advisor for the provider-pair matrix."
	if err := os.WriteFile(filepath.Join(roles, "advisor.md"), []byte(persona+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := preset.Load(s.layout.Repo, "", name)
	if err != nil {
		t.Fatal(err)
	}
	for i := range loaded.Roles {
		if loaded.Roles[i].Name == "advisor" {
			return preset.ConsultBody(&loaded.Roles[i])
		}
	}
	t.Fatal("fixture preset has no advisor role")
	return ""
}

func consultPersonaPrompt(persona, question string) string {
	return persona + "\n\n---\n\nYour question:\n\n" + question
}

func consultPairStep(target agents.Target, delivery, result, prompt, reply string) consultStepSpec {
	step := consultStepSpec{
		Provider: target.Provider, Delivery: delivery, Result: result, Prompt: prompt,
		Model: target.Model, Effort: target.Effort,
	}
	step.Reply = reply
	if target.Provider == "codex" && result == "usable" {
		step.UsageIn, step.UsageOut = 7, 3
	}
	return step
}

func processEvents(trace []*processTrace, source, event string) []*processTrace {
	var matches []*processTrace
	for _, record := range trace {
		if record.Source == source && record.Event == event {
			matches = append(matches, record)
		}
	}
	return matches
}

func assertConsultRoleWiring(t *testing.T, run *processRun, lead string, first, second agents.Target) {
	t.Helper()
	if run == nil {
		t.Fatal("runtime run trace is absent")
	}
	values := map[string]processEnv{}
	for _, item := range run.Environment {
		values[item.Name] = item
	}
	wantTargets := first.String() + " " + second.String()
	if values["COOP_PRIMARY"].Value != lead || values["COOP_CONSULT_ADVISOR_TARGETS"].Value != wantTargets {
		t.Fatalf("role scope/targets = primary %#v targets %#v, want %s / %s", values["COOP_PRIMARY"], values["COOP_CONSULT_ADVISOR_TARGETS"], lead, wantTargets)
	}
	peers := strings.Fields(values["COOP_PEERS"].Value)
	if !slices.Contains(peers, first.Provider) || !slices.Contains(peers, second.Provider) {
		t.Fatalf("role credential scope %q does not contain %s and %s", peers, first.Provider, second.Provider)
	}
	wrapper, persona := 0, 0
	for _, mount := range run.Mounts {
		if mount.Target == "<container>"+fusion.ConsultWrapperPath && mount.ReadOnly {
			wrapper++
		}
		if mount.Target == "<container>/home/node/.coop/consult/advisor.md" && mount.ReadOnly && strings.HasPrefix(mount.Source, "<root>/tmp/coop-mcp-") {
			persona++
		}
	}
	if wrapper != 1 || persona != 1 {
		t.Fatalf("role mounts = wrapper %d persona %d: %#v", wrapper, persona, run.Mounts)
	}
}

func consultProcessScenario(lead string, homes []string, calls []consultCallSpec, steps []consultStepSpec) *consultProcessSpec {
	return &consultProcessSpec{
		Version: 2, Provider: lead, ProviderHomes: homes,
		Consult: consultProcessPlan{Calls: calls, Steps: steps},
	}
}

func consultUsage(provider string, value int) int {
	if provider == "codex" {
		return value
	}
	return 0
}

func assertConsultMounts(t *testing.T, suite *directProcessSuite, run *processTrace, lead, peer string) {
	t.Helper()
	if run.Run == nil {
		t.Fatal("runtime run trace has no run")
	}
	wrapper, repo := 0, 0
	credentials := map[string]bool{}
	for _, m := range run.Run.Mounts {
		switch {
		case m.Target == "<container>"+fusion.ConsultWrapperPath:
			wrapper++
			if !m.ReadOnly || !strings.HasPrefix(m.Source, "<root>/tmp/coop-mcp-") {
				t.Errorf("unsafe consult wrapper mount: %#v", m)
			}
		case m.Source == processTracePath(suite.layout.Root, suite.layout.Repo) && m.Target == processTracePath(suite.layout.Root, suite.layout.Repo):
			repo++
			if m.ReadOnly {
				t.Errorf("deterministic consult repo must be writable: %#v", m)
			}
		default:
			for _, provider := range []string{lead, peer} {
				wantSource := processTracePath(suite.layout.Root, filepath.Join(suite.layout.Config, provider, "profiles", "personal"))
				if m.Source == wantSource && m.Target == "<container>/home/node/."+provider && !m.ReadOnly {
					credentials[provider] = true
				}
			}
		}
	}
	if wrapper != 1 || repo != 1 || !credentials[lead] || !credentials[peer] {
		t.Fatalf("consult mounts = wrapper %d repo %d credentials %#v\nall: %#v", wrapper, repo, credentials, run.Run.Mounts)
	}
	for _, provider := range suite.providers {
		if provider != lead && provider != peer {
			unexpected := processTracePath(suite.layout.Root, filepath.Join(suite.layout.Config, provider, "profiles", "personal"))
			for _, m := range run.Run.Mounts {
				if m.Source == unexpected {
					t.Errorf("out-of-scope credential mounted for %s: %#v", provider, m)
				}
			}
		}
	}
}

func assertConsultEnvironment(t *testing.T, environment []processEnv, lead, peer, model, effort string) {
	t.Helper()
	values := map[string]processEnv{}
	for _, item := range environment {
		values[item.Name] = item
	}
	if values["COOP_PRIMARY"].Value != lead || values["COOP_PEERS"].Value != peer || values["COOP_CONSULT_TIMEOUT"].Value != "2" {
		t.Fatalf("consult scope environment = primary %#v peers %#v timeout %#v", values["COOP_PRIMARY"], values["COOP_PEERS"], values["COOP_CONSULT_TIMEOUT"])
	}
	key := strings.ToUpper(strings.ReplaceAll(peer, "-", "_"))
	if values["COOP_PEER_MODEL_"+key].Value != model || values["COOP_PEER_EFFORT_"+key].Value != effort {
		t.Fatalf("consult target environment = model %#v effort %#v", values["COOP_PEER_MODEL_"+key], values["COOP_PEER_EFFORT_"+key])
	}
}

func consultFreshTraceArgv(provider, model, effort, prompt, sessionHash string) []string {
	value := processTraceValue
	switch provider {
	case "claude":
		return []string{"claude", "-p", "--permission-mode", value("plan"), "--session-id", sessionHash, "--model", value(model), "--effort", value(effort), value(prompt)}
	case "codex":
		return []string{"codex", value("exec"), "-s", value("read-only"), "--model", value(model), "-c", value("model_reasoning_effort=" + effort), "--json", value(prompt)}
	case "gemini":
		return []string{"gemini", "--approval-mode", value("plan"), "--session-id", sessionHash, "--model", value(model), "-p", value(prompt)}
	case "grok":
		return []string{"grok", "--tools", value("read_file,grep,list_dir"), "--session-id", sessionHash, "--model", value(model), "--reasoning-effort", value(effort), "-p", value(prompt)}
	default:
		panic("missing consult argv contract for " + provider)
	}
}

func processTraceValue(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("<sha256:%x>", digest[:8])
}

func TestProviderScriptedConsultRegistryCompleteness(t *testing.T) {
	providers := agents.Names()
	for _, provider := range providers {
		if _, ok := directProviderContracts[provider]; !ok {
			t.Errorf("registered provider %q has no consult model/effort contract", provider)
		}
	}
	if len(providers) != len(directProviderContracts) {
		t.Fatalf("consult contracts = %v, providers = %v", sortedKeys(directProviderContracts), providers)
	}
	if _, err := os.Stat(filepath.Join("testdata", "providerfixture")); err != nil {
		t.Fatalf("strict provider fixture is missing: %v", err)
	}
}
