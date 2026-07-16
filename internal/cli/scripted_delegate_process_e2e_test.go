//go:build providere2e

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
)

type delegateCallSpec struct {
	Role       string `json:"role"`
	Prompt     string `json:"prompt,omitempty"`
	PromptKind string `json:"prompt_kind,omitempty"`
	ExitCode   int    `json:"exit_code"`
}

type delegateStepSpec struct {
	Provider string `json:"provider"`
	Result   string `json:"result"`
	Prompt   string `json:"prompt"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

type delegateProcessPlan struct {
	Contract   string                      `json:"contract"`
	Calls      []delegateCallSpec          `json:"calls"`
	Steps      []delegateStepSpec          `json:"steps"`
	Consult    *delegateConsultProcessPlan `json:"consult,omitempty"`
	Concurrent bool                        `json:"concurrent,omitempty"`
}

type delegateConsultProcessPlan struct {
	Steps []consultStepSpec `json:"steps"`
}

type delegateProcessSpec struct {
	Version       int                 `json:"version"`
	Provider      string              `json:"provider"`
	ProviderHomes []string            `json:"provider_homes"`
	Delegate      delegateProcessPlan `json:"delegate"`
}

func TestProviderScriptedDelegateArms(t *testing.T) {
	suite := newDirectProcessSuite(t)
	providers := agents.Names()
	for _, provider := range providers {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			suite.resetDelegateRepository(t)
			lead := providerPairLead(providers, provider, provider)
			target := providerPairTarget(provider)
			name := "delegate-arm-" + provider
			contract := suite.writeDelegatePreset(t, name, lead, target)
			question := "make the deterministic fixture edit with " + provider
			prompt := delegateRolePrompt(contract, question)
			baseline, err := liveprovider.SnapshotRepository(suite.layout)
			if err != nil {
				t.Fatal(err)
			}
			scenario := delegateProcessScenario(lead, delegateHomes(lead, target), contract,
				[]delegateCallSpec{{Role: "worker", Prompt: question, ExitCode: 0}},
				[]delegateStepSpec{delegateTargetStep(target, "edit", prompt)},
			)
			result, trace := suite.run(t, []string{name}, scenario)
			if result.Err != nil || result.ExitCode != 0 {
				t.Fatalf("delegate arm %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", provider, result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
			}
			after, err := liveprovider.SnapshotRepository(suite.layout)
			if err != nil {
				t.Fatal(err)
			}
			assertDelegateHistoryUnchanged(t, baseline, after)
			wantStatus := "?? delegate-edit-" + provider + ".txt"
			if strings.TrimSpace(after.Status) != wantStatus {
				t.Fatalf("delegate arm status = %q, want %q", after.Status, wantStatus)
			}
			run := oneProcessEvent(t, trace, "runtime", "run")
			assertDelegateRoleWiring(t, run.Run, lead, []agents.Target{target})
			assertDelegatePeerSequence(t, trace, []delegateStepSpec{delegateTargetStep(target, "edit", prompt)})
			assertProcessTraceGone(t, trace)
		})
	}
}

func TestProviderScriptedDelegateDirectedFallbackPairs(t *testing.T) {
	suite := newDirectProcessSuite(t)
	providers := agents.Names()
	pairs := 0
	for _, first := range providers {
		for _, second := range providers {
			if first == second {
				continue
			}
			first, second := first, second
			pairs++
			t.Run(first+"_to_"+second, func(t *testing.T) {
				suite.resetDelegateRepository(t)
				lead := providerPairLead(providers, first, second)
				firstTarget, secondTarget := providerPairTarget(first), providerPairTarget(second)
				name := "delegate-pair-" + first + "-" + second
				contract := suite.writeDelegatePreset(t, name, lead, firstTarget, secondTarget)
				recoveryQuestion := "recover the clean limited delegate " + first + " to " + second
				exhaustionQuestion := "exhaust the delegate pair " + first + " to " + second
				recoveryPrompt := delegateRolePrompt(contract, recoveryQuestion)
				exhaustionPrompt := delegateRolePrompt(contract, exhaustionQuestion)
				steps := []delegateStepSpec{
					delegateTargetStep(firstTarget, "limited", recoveryPrompt),
					delegateTargetStep(secondTarget, "success", recoveryPrompt),
					delegateTargetStep(firstTarget, "limited", exhaustionPrompt),
					delegateTargetStep(secondTarget, "limited", exhaustionPrompt),
				}
				baseline, err := liveprovider.SnapshotRepository(suite.layout)
				if err != nil {
					t.Fatal(err)
				}
				result, trace := suite.run(t, []string{name}, delegateProcessScenario(lead, delegateHomes(lead, firstTarget, secondTarget), contract,
					[]delegateCallSpec{
						{Role: "worker", Prompt: recoveryQuestion, ExitCode: 0},
						{Role: "worker", Prompt: exhaustionQuestion, ExitCode: 75},
					}, steps))
				if result.Err != nil || result.ExitCode != 75 || !strings.Contains(result.Stdout, "fixture delegate success "+second) {
					t.Fatalf("delegate pair %s -> %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", first, second, result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
				}
				after, err := liveprovider.SnapshotRepository(suite.layout)
				if err != nil {
					t.Fatal(err)
				}
				assertDelegateHistoryUnchanged(t, baseline, after)
				if after.Status != baseline.Status {
					t.Fatalf("clean delegate fallback pair changed status: %q -> %q", baseline.Status, after.Status)
				}
				run := oneProcessEvent(t, trace, "runtime", "run")
				assertDelegateRoleWiring(t, run.Run, lead, []agents.Target{firstTarget, secondTarget})
				assertDelegatePeerSequence(t, trace, steps)
				assertProcessTraceGone(t, trace)
			})
		}
	}
	if pairs != 12 {
		t.Fatalf("directed delegate pairs = %d, want 12", pairs)
	}
}

func TestProviderScriptedDelegateSafety(t *testing.T) {
	suite := newDirectProcessSuite(t)
	lead := "claude"
	first, second := providerPairTarget("codex"), providerPairTarget("gemini")
	cases := []struct {
		name     string
		result   string
		exitCode int
		dirty    bool
	}{
		{"ordinary failure", "ordinary", 23, false},
		{"limited tracked edit", "limited-edit", 75, false},
		{"limited staged edit", "limited-staged", 75, false},
		{"limited ignored edit", "limited-ignored", 75, false},
		{"limited ref mutation", "limited-ref", 3, false},
		{"limited commit reset", "limited-commit-reset", 3, false},
		{"direct commit", "commit", 3, false},
		{"direct commit reset", "commit-reset", 3, false},
		{"dirty baseline", "limited", 75, true},
		{"timeout", "wait", 124, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			suite.resetDelegateRepository(t)
			contract := suite.writeDelegatePreset(t, "delegate-safety", lead, first, second)
			if tc.dirty {
				if err := os.WriteFile(filepath.Join(suite.layout.Repo, "preexisting-dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			question := "exercise delegate safety: " + tc.name
			step := delegateTargetStep(first, tc.result, delegateRolePrompt(contract, question))
			baseline, err := liveprovider.SnapshotRepository(suite.layout)
			if err != nil {
				t.Fatal(err)
			}
			result, trace := suite.run(t, []string{"delegate-safety"}, delegateProcessScenario(lead, delegateHomes(lead, first, second), contract,
				[]delegateCallSpec{{Role: "worker", Prompt: question, ExitCode: tc.exitCode}}, []delegateStepSpec{step}))
			if result.Err != nil || result.ExitCode != tc.exitCode {
				t.Fatalf("delegate safety %s = exit %d err %v, want %d\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", tc.name, result.ExitCode, result.Err, tc.exitCode, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
			}
			after, err := liveprovider.SnapshotRepository(suite.layout)
			if err != nil {
				t.Fatal(err)
			}
			assertDelegateSafetyEvidence(t, suite, tc.result, baseline, after)
			assertDelegatePeerSequence(t, trace, []delegateStepSpec{step})
			assertProcessTraceGone(t, trace)
		})
	}

	t.Run("recursive delegate denied", func(t *testing.T) {
		suite.resetDelegateRepository(t)
		contract := suite.writeDelegatePreset(t, "delegate-recursive", lead, first)
		question := "attempt one nested delegate"
		step := delegateTargetStep(first, "recursive", delegateRolePrompt(contract, question))
		result, trace := suite.run(t, []string{"delegate-recursive"}, delegateProcessScenario(lead, delegateHomes(lead, first), contract,
			[]delegateCallSpec{{Role: "worker", Prompt: question, ExitCode: 0}}, []delegateStepSpec{step}))
		if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout+result.Stderr, "recursive delegation is not allowed") {
			t.Fatalf("recursive delegate = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		assertDelegatePeerSequence(t, trace, []delegateStepSpec{step})
	})

	t.Run("all targets unmounted", func(t *testing.T) {
		suite.resetDelegateRepository(t)
		disableProcessCredential(t, suite, first.Provider)
		disableProcessCredential(t, suite, second.Provider)
		contract := suite.writeDelegatePreset(t, "delegate-unmounted", lead, first, second)
		result, trace := suite.run(t, []string{"delegate-unmounted"}, delegateProcessScenario(lead, []string{lead}, contract,
			[]delegateCallSpec{{Role: "worker", Prompt: "no mounted target", ExitCode: 2}}, nil))
		if result.Err != nil || result.ExitCode != 2 || !strings.Contains(result.Stderr, "no target with mounted credentials") {
			t.Fatalf("unmounted delegate = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if starts := processEvents(trace, "delegate-peer", "start"); len(starts) != 0 {
			t.Fatalf("unmounted delegate dispatched %d providers", len(starts))
		}
	})
}

func TestProviderScriptedDelegateCanConsult(t *testing.T) {
	suite := newDirectProcessSuite(t)
	suite.resetDelegateRepository(t)
	lead := "claude"
	worker, advisor := providerPairTarget("codex"), providerPairTarget("gemini")
	contract, persona := suite.writeDelegateConsultPreset(t, "delegate-consult", lead, worker, advisor)
	question := "ask the configured advisor while implementing"
	delegateStep := delegateTargetStep(worker, "consult", delegateRolePrompt(contract, question))
	consultQuestion := "delegate nested question"
	consultStep := consultPairStep(advisor, "fresh", "usable", consultPersonaPrompt(persona, consultQuestion), "nested consult reply")
	scenario := delegateProcessScenario(lead, delegateHomes(lead, worker, advisor), contract,
		[]delegateCallSpec{{Role: "worker", Prompt: question, ExitCode: 0}}, []delegateStepSpec{delegateStep})
	scenario.Delegate.Consult = &delegateConsultProcessPlan{Steps: []consultStepSpec{consultStep}}
	result, trace := suite.run(t, []string{"delegate-consult"}, scenario)
	if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "nested consult reply") {
		t.Fatalf("delegate consult = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
	}
	assertDelegatePeerSequence(t, trace, []delegateStepSpec{delegateStep})
	peers := processEvents(trace, "peer", "start")
	if len(peers) != 1 || peers[0].Consult == nil || peers[0].Consult.Provider != advisor.Provider || peers[0].Consult.PromptHash != processTraceValue(consultStep.Prompt) {
		t.Fatalf("nested consult trace = %#v", peers)
	}
	assertProcessTraceGone(t, trace)
}

func TestProviderScriptedDelegateSerialization(t *testing.T) {
	suite := newDirectProcessSuite(t)
	suite.resetDelegateRepository(t)
	lead := "claude"
	worker := providerPairTarget("codex")
	contract := suite.writeDelegatePreset(t, "delegate-serialized", lead, worker)
	firstQuestion, secondQuestion := "hold the delegate lock", "run after the lock releases"
	steps := []delegateStepSpec{
		delegateTargetStep(worker, "wait", delegateRolePrompt(contract, firstQuestion)),
		delegateTargetStep(worker, "success", delegateRolePrompt(contract, secondQuestion)),
	}
	scenario := delegateProcessScenario(lead, delegateHomes(lead, worker), contract,
		[]delegateCallSpec{
			{Role: "worker", Prompt: firstQuestion, ExitCode: 0},
			{Role: "worker", Prompt: secondQuestion, ExitCode: 0},
		}, steps)
	scenario.Delegate.Concurrent = true
	baseline, err := liveprovider.SnapshotRepository(suite.layout)
	if err != nil {
		t.Fatal(err)
	}
	result, trace := suite.run(t, []string{"delegate-serialized"}, scenario)
	if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "fixture delegate success "+worker.Provider) {
		t.Fatalf("serialized delegates = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
	}
	after, err := liveprovider.SnapshotRepository(suite.layout)
	if err != nil {
		t.Fatal(err)
	}
	assertDelegateHistoryUnchanged(t, baseline, after)
	if baseline.Status != after.Status {
		t.Fatalf("serialized delegates changed status: %q -> %q", baseline.Status, after.Status)
	}
	assertDelegatePeerSequence(t, trace, steps)
	firstExit, secondStart := 0, 0
	for _, event := range trace {
		if event.Source == "delegate-peer" && event.Delegate != nil && event.Delegate.Step == 0 && event.Event == "exit" {
			firstExit = event.Sequence
		}
		if event.Source == "delegate-peer" && event.Delegate != nil && event.Delegate.Step == 1 && event.Event == "start" {
			secondStart = event.Sequence
		}
	}
	if firstExit == 0 || secondStart <= firstExit {
		t.Fatalf("serialized delegate order = first exit %d, second start %d", firstExit, secondStart)
	}
	assertProcessTraceGone(t, trace)
}

func delegateProcessScenario(lead string, homes []string, contract string, calls []delegateCallSpec, steps []delegateStepSpec) delegateProcessSpec {
	return delegateProcessSpec{
		Version: 3, Provider: lead, ProviderHomes: homes,
		Delegate: delegateProcessPlan{Contract: contract, Calls: calls, Steps: steps},
	}
}

func delegateHomes(lead string, targets ...agents.Target) []string {
	homes := []string{lead}
	for _, target := range targets {
		if !slices.Contains(homes, target.Provider) {
			homes = append(homes, target.Provider)
		}
	}
	return homes
}

func delegateTargetStep(target agents.Target, result, prompt string) delegateStepSpec {
	return delegateStepSpec{Provider: target.Provider, Result: result, Prompt: prompt, Model: target.Model, Effort: target.Effort}
}

func delegateRolePrompt(contract, question string) string {
	return contract + "\n\n---\n\nYour task:\n\n" + question
}

func (s *directProcessSuite) writeDelegatePreset(t *testing.T, name, lead string, targets ...agents.Target) string {
	t.Helper()
	if len(targets) == 0 {
		t.Fatal("delegate fixture preset needs a target")
	}
	dir := filepath.Join(s.layout.Repo, ".agent", "presets", name)
	roles := filepath.Join(dir, "roles")
	if err := os.MkdirAll(roles, 0o700); err != nil {
		t.Fatal(err)
	}
	values := make([]string, len(targets))
	for index, target := range targets {
		values[index] = target.String()
	}
	agentValue := values[0]
	if len(values) > 1 {
		agentValue = "[" + strings.Join(values, ", ") + "]"
	}
	body := fmt.Sprintf("lead: {agent: %s}\nroles:\n  worker:\n    mode: delegate\n    agent: %s\n    prompt: roles/worker.md\n", lead, agentValue)
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roles, "worker.md"), []byte("Follow only the closed deterministic fixture task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := preset.Load(s.layout.Repo, "", name)
	if err != nil {
		t.Fatal(err)
	}
	for index := range loaded.Roles {
		if loaded.Roles[index].Name == "worker" {
			return preset.RoleContract(&loaded.Roles[index])
		}
	}
	t.Fatal("fixture preset has no worker role")
	return ""
}

func (s *directProcessSuite) writeDelegateConsultPreset(t *testing.T, name, lead string, worker, advisor agents.Target) (string, string) {
	t.Helper()
	dir := filepath.Join(s.layout.Repo, ".agent", "presets", name)
	roles := filepath.Join(dir, "roles")
	if err := os.MkdirAll(roles, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("lead: {agent: %s}\nroles:\n  worker:\n    mode: delegate\n    agent: %s\n    prompt: roles/worker.md\n  advisor:\n    mode: consult\n    agent: %s\n    prompt: roles/advisor.md\n", lead, worker.String(), advisor.String())
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roles, "worker.md"), []byte("Use the configured read-only advisor when the fixture asks.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roles, "advisor.md"), []byte("Return only the deterministic nested consult reply.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := preset.Load(s.layout.Repo, "", name)
	if err != nil {
		t.Fatal(err)
	}
	var contract, persona string
	for index := range loaded.Roles {
		switch loaded.Roles[index].Name {
		case "worker":
			contract = preset.RoleContract(&loaded.Roles[index])
		case "advisor":
			persona = preset.ConsultBody(&loaded.Roles[index])
		}
	}
	if contract == "" || persona == "" {
		t.Fatal("fixture preset is missing its delegate or consult contract")
	}
	return contract, persona
}

func (s *directProcessSuite) resetDelegateRepository(t *testing.T) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = s.layout.Repo, s.env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("reset", "--hard", s.repoHead)
	for _, pattern := range []string{"delegate-*.txt", "preexisting-dirty.txt", ".delegate-ignored"} {
		matches, err := filepath.Glob(filepath.Join(s.layout.Repo, pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range matches {
			if err := os.Remove(match); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}
	tags := exec.Command("git", "tag", "--list", "fixture-delegate-*")
	tags.Dir, tags.Env = s.layout.Repo, s.env
	out, err := tags.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range strings.Fields(string(out)) {
		run("tag", "-d", tag)
	}
}

func assertDelegateHistoryUnchanged(t *testing.T, before, after liveprovider.RepositorySnapshot) {
	t.Helper()
	if before.Head != after.Head || before.Refs != after.Refs || before.Reflog != after.Reflog {
		t.Fatalf("delegate mutated history: HEAD %q -> %q", before.Head, after.Head)
	}
}

func assertDelegateSafetyEvidence(t *testing.T, suite *directProcessSuite, result string, before, after liveprovider.RepositorySnapshot) {
	t.Helper()
	switch result {
	case "ordinary", "limited", "wait":
		assertDelegateHistoryUnchanged(t, before, after)
		if before.Status != after.Status {
			t.Fatalf("delegate %s changed status: %q -> %q", result, before.Status, after.Status)
		}
	case "limited-edit":
		assertDelegateHistoryUnchanged(t, before, after)
		if !strings.Contains(after.Status, "delegate-partial.txt") {
			t.Fatalf("limited tracked edit evidence is absent: %q", after.Status)
		}
	case "limited-staged":
		assertDelegateHistoryUnchanged(t, before, after)
		if !strings.Contains(after.Status, "A  delegate-staged.txt") {
			t.Fatalf("limited staged edit evidence is absent: %q", after.Status)
		}
	case "limited-ignored":
		assertDelegateHistoryUnchanged(t, before, after)
		if _, err := os.Stat(filepath.Join(suite.layout.Repo, ".delegate-ignored")); err != nil {
			t.Fatalf("limited ignored edit evidence is absent: %v", err)
		}
	case "limited-ref":
		if before.Refs == after.Refs || !strings.Contains(after.Refs, "fixture-delegate-mutated") {
			t.Fatal("limited ref mutation evidence is absent")
		}
	case "limited-commit-reset", "commit-reset":
		if before.Head != after.Head || before.Reflog == after.Reflog {
			t.Fatalf("%s did not preserve its reset reflog evidence", result)
		}
	case "commit":
		if before.Head == after.Head {
			t.Fatal("direct commit evidence is absent")
		}
	default:
		t.Fatalf("missing evidence assertion for delegate result %q", result)
	}
}

func assertDelegateRoleWiring(t *testing.T, run *processRun, lead string, targets []agents.Target) {
	t.Helper()
	if run == nil {
		t.Fatal("runtime run trace is absent")
	}
	values := map[string]processEnv{}
	for _, item := range run.Environment {
		values[item.Name] = item
	}
	wantTargets := make([]string, len(targets))
	for index, target := range targets {
		wantTargets[index] = target.String()
	}
	if values["COOP_PRIMARY"].Value != lead || values["COOP_DELEGATE_WORKER_TARGETS"].Value != strings.Join(wantTargets, " ") {
		t.Fatalf("delegate role scope/targets = primary %#v targets %#v", values["COOP_PRIMARY"], values["COOP_DELEGATE_WORKER_TARGETS"])
	}
	peers := strings.Fields(values["COOP_PEERS"].Value)
	wantPeers := []string{}
	for _, target := range targets {
		if target.Provider != lead && !slices.Contains(wantPeers, target.Provider) {
			wantPeers = append(wantPeers, target.Provider)
		}
	}
	slices.Sort(peers)
	slices.Sort(wantPeers)
	if !slices.Equal(peers, wantPeers) {
		t.Fatalf("delegate credential scope = %q, want exactly %q", peers, wantPeers)
	}
	wantHomes := append([]string{lead}, wantPeers...)
	gotHomes := []string{}
	for _, mount := range run.Mounts {
		provider := strings.TrimPrefix(mount.Target, "<container>/home/node/.")
		if provider != mount.Target && slices.Contains(agents.Names(), provider) && !slices.Contains(gotHomes, provider) {
			gotHomes = append(gotHomes, provider)
		}
	}
	slices.Sort(gotHomes)
	slices.Sort(wantHomes)
	if !slices.Equal(gotHomes, wantHomes) {
		t.Fatalf("delegate credential mounts = %q, want exactly %q", gotHomes, wantHomes)
	}
	wrapper, contract := 0, 0
	for _, mount := range run.Mounts {
		if mount.Target == "<container>"+preset.DelegateWrapperPath && mount.ReadOnly {
			wrapper++
		}
		if mount.Target == "<container>/home/node/.coop/delegate/worker.md" && mount.ReadOnly {
			contract++
		}
	}
	if wrapper != 1 || contract != 1 {
		t.Fatalf("delegate role mounts = wrapper %d contract %d: %#v", wrapper, contract, run.Mounts)
	}
}

func assertDelegatePeerSequence(t *testing.T, trace []*processTrace, want []delegateStepSpec) {
	t.Helper()
	starts := processEvents(trace, "delegate-peer", "start")
	if len(starts) != len(want) {
		t.Fatalf("delegate peer starts = %d, want %d", len(starts), len(want))
	}
	for index, expected := range want {
		got := starts[index]
		if got.Delegate == nil || got.Delegate.Provider != expected.Provider || got.Delegate.Result != expected.Result ||
			got.Delegate.Model != expected.Model || got.Delegate.Effort != expected.Effort || got.Delegate.PromptHash != processTraceValue(expected.Prompt) {
			t.Fatalf("delegate peer %d metadata = %#v, want %#v", index, got.Delegate, expected)
		}
		if processEnvironmentValue(got.Environment, preset.DelegateDepthEnv) != "1" {
			t.Fatalf("delegate peer %d depth is not 1: %#v", index, got.Environment)
		}
	}
}

func assertProcessTraceGone(t *testing.T, trace []*processTrace) {
	t.Helper()
	for _, event := range trace {
		if event.PID > 0 {
			awaitProcessGone(t, event.PID)
		}
	}
}
