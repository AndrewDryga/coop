//go:build providere2e

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
)

func TestProviderScriptedFusionCompositionMatrix(t *testing.T) {
	suite := newDirectProcessSuite(t)
	providers := agents.Names()
	for _, lead := range providers {
		t.Run(lead, func(t *testing.T) {
			presetName := "composition-" + lead
			leadTarget := compositionTarget(lead, "lead")
			roleTargets := make(map[string]agents.Target, len(providers))
			roles := make([]preset.Role, 0, len(providers))
			for _, provider := range providers {
				target := compositionTarget(provider, "role")
				roleTargets[provider] = target
				roles = append(roles, preset.Role{
					Name: compositionRole(provider), Mode: preset.ModeConsult, Ladder: []agents.Target{target},
					PromptText: "You are " + compositionRole(provider) + " in the deterministic Fusion composition matrix.",
				})
			}
			personas := writeFusionRolePreset(t, suite.layout.Repo, presetName, leadTarget, roles)
			baseline, err := liveprovider.SnapshotRepository(suite.layout)
			if err != nil {
				t.Fatal(err)
			}
			var calls []consultCallSpec
			var steps []consultStepSpec
			var replies []string
			var members []string
			for _, provider := range providers {
				role := compositionRole(provider)
				question := "composition question from " + lead + " to " + provider
				reply := "composition reply from " + provider + " to " + lead
				members = append(members, role)
				calls = append(calls, consultCallSpec{Target: role, Mode: "fresh", Prompt: question, ExitCode: 0})
				steps = append(steps, consultPairStep(roleTargets[provider], "fresh", "usable", consultPersonaPrompt(personas[role], question), reply))
				replies = append(replies, reply)
			}

			result, trace := suite.run(t, []string{"fusion", presetName}, consultProcessScenario(lead, providers, calls, steps))
			if result.Err != nil || result.ExitCode != 0 {
				t.Fatalf("fusion composition %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", lead, result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
			}
			for _, reply := range replies {
				if !strings.Contains(result.Stdout, reply) {
					t.Errorf("fusion composition %s missing reply %q\nstdout:\n%s", lead, reply, result.Stdout)
				}
			}
			wantCouncil := "fusion: " + lead + " governs; council " + strings.Join(members, " + ") + " (read-only)"
			if !strings.Contains(result.Stderr, wantCouncil) {
				t.Errorf("fusion council identity missing %q\nstderr:\n%s", wantCouncil, result.Stderr)
			}
			after, err := liveprovider.VerifyRepository(suite.layout, baseline)
			if err != nil {
				t.Fatal(err)
			}
			if !baseline.Equal(after) {
				t.Fatal("fusion composition changed the writable repository")
			}

			run := oneProcessEvent(t, trace, "runtime", "run")
			assertFusionCompositionRun(t, suite, run, leadTarget, roleTargets, members)
			starts := processEvents(trace, "peer", "start")
			if len(starts) != len(steps) {
				t.Fatalf("fusion composition %s started %d roles, want %d", lead, len(starts), len(steps))
			}
			for index, start := range starts {
				want := steps[index]
				if start.Consult == nil || start.Consult.Step != index || start.Consult.Provider != want.Provider ||
					start.Consult.Model != want.Model || start.Consult.Effort != want.Effort ||
					start.Consult.PromptHash != processTraceValue(want.Prompt) {
					t.Errorf("fusion role start %d = %#v, want %#v", index, start.Consult, want)
					continue
				}
				wantArgv := consultFreshTraceArgv(want.Provider, want.Model, want.Effort, want.Prompt, start.Consult.SessionHash)
				if !reflect.DeepEqual(start.Argv, wantArgv) {
					t.Errorf("fusion role %d argv = %q, want %q", index, start.Argv, wantArgv)
				}
			}
			for _, member := range members {
				assertConsultStateSecure(t, suite, member, true)
			}
			for _, event := range trace {
				if event.PID > 0 {
					awaitProcessGone(t, event.PID)
				}
			}
		})
	}
}

func TestProviderScriptedFusionExplicitPeerAndRoleIdentities(t *testing.T) {
	suite := newDirectProcessSuite(t)
	lead, peer := suite.providers[0], suite.providers[1]
	name := "peer-and-roles"
	leadTarget := compositionTarget(lead, "mixed-lead")
	peerTarget := compositionTarget(peer, "explicit-peer")
	roleTargets := map[string]agents.Target{
		"analyst": compositionTarget(peer, "analyst"),
		"critic":  compositionTarget(peer, "critic"),
	}
	personas := writeFusionRolePreset(t, suite.layout.Repo, name, leadTarget, []preset.Role{
		{Name: "analyst", Mode: preset.ModeConsult, Ladder: []agents.Target{roleTargets["analyst"]}, PromptText: "Deterministic analyst persona."},
		{Name: "critic", Mode: preset.ModeConsult, Ladder: []agents.Target{roleTargets["critic"]}, PromptText: "Deterministic critic persona."},
	})
	calls := []consultCallSpec{
		{Target: peer, Mode: "fresh", Prompt: "explicit peer question", ExitCode: 0},
		{Target: "analyst", Mode: "fresh", Prompt: "analyst question", ExitCode: 0},
		{Target: "critic", Mode: "fresh", Prompt: "critic question", ExitCode: 0},
	}
	steps := []consultStepSpec{
		consultPairStep(peerTarget, "fresh", "usable", calls[0].Prompt, "explicit peer reply"),
		consultPairStep(roleTargets["analyst"], "fresh", "usable", consultPersonaPrompt(personas["analyst"], calls[1].Prompt), "analyst reply"),
		consultPairStep(roleTargets["critic"], "fresh", "usable", consultPersonaPrompt(personas["critic"], calls[2].Prompt), "critic reply"),
	}
	result, trace := suite.run(t, []string{"fusion", name, "--peer", peerTarget.String()}, consultProcessScenario(lead, suite.providers, calls, steps))
	wantCouncil := "fusion: " + lead + " governs; council " + peer + " + analyst + critic (read-only)"
	if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stderr, wantCouncil) {
		t.Fatalf("mixed Fusion = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
	}
	starts := processEvents(trace, "peer", "start")
	if len(starts) != len(steps) {
		t.Fatalf("mixed Fusion peer starts = %d, want %d", len(starts), len(steps))
	}
	for index, start := range starts {
		want := steps[index]
		if start.Consult == nil || start.Consult.Provider != peer || start.Consult.Model != want.Model ||
			start.Consult.Effort != want.Effort || start.Consult.PromptHash != processTraceValue(want.Prompt) {
			t.Errorf("mixed Fusion step %d = %#v, want %#v", index, start.Consult, want)
		}
	}
	if peerTarget.Model == roleTargets["analyst"].Model || peerTarget.Model == roleTargets["critic"].Model || roleTargets["analyst"].Model == roleTargets["critic"].Model {
		t.Fatal("mixed Fusion targets must use three distinct models on the shared provider")
	}
	run := oneProcessEvent(t, trace, "runtime", "run")
	assertFusionMixedRoleWiring(t, suite, run, leadTarget, peer, peerTarget, roleTargets)
}

func TestProviderScriptedNativeRoleDegradesUnderIncapableLead(t *testing.T) {
	suite := newDirectProcessSuite(t)
	var lead, nativeProvider string
	for _, provider := range suite.providers {
		ag, _ := agents.Get(provider)
		support := ag.NativeSubagents()
		if support.HomeDir != "" && support.Render != nil {
			nativeProvider = provider
		} else if lead == "" {
			lead = provider
		}
	}
	if lead == "" || nativeProvider == "" || lead == nativeProvider {
		t.Fatalf("fixture needs distinct capable/incapable native providers, got lead=%q native=%q", lead, nativeProvider)
	}
	name := "native-degradation"
	leadTarget := compositionTarget(lead, "degraded-lead")
	roleTarget := compositionTarget(nativeProvider, "native-role")
	personas := writeFusionRolePreset(t, suite.layout.Repo, name, leadTarget, []preset.Role{{
		Name: "thinker", Mode: preset.ModeNative, Ladder: []agents.Target{roleTarget}, PromptText: "Deterministic degraded native persona.",
	}})
	question := "degraded native question"
	step := consultPairStep(roleTarget, "fresh", "usable", consultPersonaPrompt(personas["thinker"], question), "degraded native reply")
	result, trace := suite.run(t, []string{name}, consultProcessScenario(lead, suite.providers,
		[]consultCallSpec{{Target: "thinker", Mode: "fresh", Prompt: question, ExitCode: 0}}, []consultStepSpec{step}))
	if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, step.Reply) {
		t.Fatalf("native degradation = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
	}
	start := oneProcessEvent(t, trace, "peer", "start")
	if start.Consult == nil || start.Consult.Provider != nativeProvider || start.Consult.Model != roleTarget.Model || start.Consult.Effort != roleTarget.Effort {
		t.Fatalf("degraded native start = %#v, want %s", start.Consult, roleTarget.String())
	}
	run := oneProcessEvent(t, trace, "runtime", "run")
	values := processEnvironment(run.Run.Environment)
	if values["COOP_CONSULT_THINKER_TARGETS"].Value != roleTarget.String() {
		t.Errorf("degraded native target env = %#v, want %s", values["COOP_CONSULT_THINKER_TARGETS"], roleTarget.String())
	}
	persona, nativeMount := 0, 0
	for _, mount := range run.Run.Mounts {
		if mount.Target == "<container>/home/node/.coop/consult/thinker.md" {
			persona++
		}
		if strings.HasSuffix(mount.Target, "/agents") {
			nativeMount++
		}
	}
	if persona != 1 || nativeMount != 0 {
		t.Fatalf("degraded native mounts = persona %d native-dir %d", persona, nativeMount)
	}
}

func TestProviderScriptedFusionTerminalPinsFirstLeadRung(t *testing.T) {
	suite := newDirectProcessSuite(t)
	first, second, roleProvider := suite.providers[0], suite.providers[1], suite.providers[2]
	name := "terminal-first-rung"
	firstTarget := compositionTarget(first, "first-rung")
	secondTarget := compositionTarget(second, "second-rung")
	roleTarget := compositionTarget(roleProvider, "terminal-role")
	dir := filepath.Join(suite.layout.Repo, ".agent", "presets", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("lead: {agent: [%s, %s]}\nroles:\n  critic:\n    mode: consult\n    agent: %s\n", firstTarget.String(), secondTarget.String(), roleTarget.String())
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	question := "terminal rung question"
	step := consultPairStep(roleTarget, "fresh", "usable", question, "terminal rung reply")
	result, trace := suite.run(t, []string{"fusion", name}, consultProcessScenario(first, suite.providers,
		[]consultCallSpec{{Target: "critic", Mode: "fresh", Prompt: question, ExitCode: 0}}, []consultStepSpec{step}))
	if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stderr, "pins this terminal session to first lead rung "+firstTarget.String()+" (no fallback rotation") {
		t.Fatalf("terminal first rung = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
	}
	runs := processEvents(trace, "runtime", "run")
	if len(runs) != 1 || runs[0].Run == nil || runs[0].Run.Provider != first {
		t.Fatalf("terminal preset runtime runs = %#v, want only %s", runs, first)
	}
	wantArgv := processTraceArgv(directExpectedArgv(directProviderContracts[first], firstTarget.Model, firstTarget.Effort, nil))
	if !reflect.DeepEqual(runs[0].Run.ProviderArgv, wantArgv) {
		t.Errorf("terminal first-rung argv = %q, want %q", runs[0].Run.ProviderArgv, wantArgv)
	}
}

func TestProviderScriptedFusionMissingRoleAuthStopsBeforeRuntime(t *testing.T) {
	suite := newDirectProcessSuite(t)
	lead, roleProvider := suite.providers[0], suite.providers[1]
	disableProcessCredential(t, suite, roleProvider)
	name := "missing-role-auth"
	writeSingleConsultPreset(t, suite.layout.Repo, name, lead, roleProvider)
	result, trace := suite.run(t, []string{"fusion", name}, processScenario(lead, nil, 0, ""))
	if result.Err != nil || result.ExitCode != 2 || len(processEvents(trace, "runtime", "run")) != 0 ||
		len(processEvents(trace, "provider", "start")) != 0 || len(processEvents(trace, "peer", "start")) != 0 ||
		!strings.Contains(result.Stderr, "preset council role(s) advisor have no target with mounted credentials") {
		t.Fatalf("missing role auth = exit %d err %v\nstderr:\n%s\ntrace:\n%s", result.ExitCode, result.Err, result.Stderr, readProcessFile(t, suite.layout.Trace))
	}
}

func compositionRole(provider string) string { return "role-" + provider }

func compositionTarget(provider, purpose string) agents.Target {
	raw := provider + ":composition-" + purpose + "-" + provider
	if directProviderContracts[provider].supportsEffort {
		raw += "/high"
	}
	target, err := agents.ParseTarget(raw)
	if err != nil {
		panic(err)
	}
	return target
}

func writeFusionRolePreset(t *testing.T, repo, name string, lead agents.Target, roles []preset.Role) map[string]string {
	t.Helper()
	dir := filepath.Join(repo, ".agent", "presets", name)
	rolesDir := filepath.Join(dir, "roles")
	if err := os.MkdirAll(rolesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	fmt.Fprintf(&body, "lead: {agent: %s}\nroles:\n", lead.String())
	for index := range roles {
		role := &roles[index]
		if len(role.Ladder) != 1 {
			t.Fatalf("fixture role %q needs exactly one target", role.Name)
		}
		fmt.Fprintf(&body, "  %s:\n    mode: %s\n    agent: %s\n    prompt: roles/%s.md\n", role.Name, role.Mode, role.Ladder[0].String(), role.Name)
		if err := os.WriteFile(filepath.Join(rolesDir, role.Name+".md"), []byte(role.PromptText+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := preset.Load(repo, "", name)
	if err != nil {
		t.Fatal(err)
	}
	personas := make(map[string]string, len(loaded.Roles))
	for index := range loaded.Roles {
		personas[loaded.Roles[index].Name] = preset.ConsultBody(&loaded.Roles[index])
	}
	return personas
}

func processEnvironment(environment []processEnv) map[string]processEnv {
	values := make(map[string]processEnv, len(environment))
	for _, item := range environment {
		values[item.Name] = item
	}
	return values
}

func assertFusionMixedRoleWiring(t *testing.T, suite *directProcessSuite, event *processTrace, lead agents.Target, peer string, peerTarget agents.Target, roles map[string]agents.Target) {
	t.Helper()
	if event.Run == nil {
		t.Fatal("mixed Fusion runtime trace has no run")
	}
	wantLeadArgv := processTraceArgv(directExpectedArgv(directProviderContracts[lead.Provider], lead.Model, lead.Effort, nil))
	if event.Run.Provider != lead.Provider || !reflect.DeepEqual(event.Run.ProviderArgv, wantLeadArgv) {
		t.Fatalf("mixed Fusion lead = %s %q, want %s %q", event.Run.Provider, event.Run.ProviderArgv, lead.Provider, wantLeadArgv)
	}
	values := processEnvironment(event.Run.Environment)
	peerKey := strings.ToUpper(strings.ReplaceAll(peer, "-", "_"))
	if values["COOP_PEER_MODEL_"+peerKey].Value != peerTarget.Model || values["COOP_PEER_EFFORT_"+peerKey].Value != peerTarget.Effort {
		t.Errorf("explicit peer target env = model %#v effort %#v", values["COOP_PEER_MODEL_"+peerKey], values["COOP_PEER_EFFORT_"+peerKey])
	}
	for role, target := range roles {
		key := strings.ToUpper(strings.ReplaceAll(role, "-", "_"))
		if values["COOP_CONSULT_"+key+"_TARGETS"].Value != target.String() {
			t.Errorf("mixed role %s target = %#v, want %s", role, values["COOP_CONSULT_"+key+"_TARGETS"], target.String())
		}
	}
	credentials := map[string]int{}
	for _, mount := range event.Run.Mounts {
		for _, provider := range suite.providers {
			want := processTracePath(suite.layout.Root, filepath.Join(suite.layout.Config, provider, "profiles", "personal"))
			if mount.Source == want {
				credentials[provider]++
			}
		}
	}
	if credentials[lead.Provider] != 1 || credentials[peer] != 1 {
		t.Fatalf("mixed Fusion credential scope = %#v, want only %s and %s once", credentials, lead.Provider, peer)
	}
	for _, provider := range suite.providers {
		if provider != lead.Provider && provider != peer && credentials[provider] != 0 {
			t.Errorf("mixed Fusion mounted out-of-scope %s credentials", provider)
		}
	}
}

func assertFusionCompositionRun(t *testing.T, suite *directProcessSuite, event *processTrace, lead agents.Target, roles map[string]agents.Target, members []string) {
	t.Helper()
	if event.Run == nil {
		t.Fatal("fusion composition runtime trace has no run")
	}
	wantLeadArgv := processTraceArgv(directExpectedArgv(directProviderContracts[lead.Provider], lead.Model, lead.Effort, nil))
	if event.Run.Provider != lead.Provider || !reflect.DeepEqual(event.Run.ProviderArgv, wantLeadArgv) {
		t.Fatalf("fusion lead = %s %q, want %s %q", event.Run.Provider, event.Run.ProviderArgv, lead.Provider, wantLeadArgv)
	}
	values := processEnvironment(event.Run.Environment)
	if values["COOP_PRIMARY"].Value != lead.Provider {
		t.Fatalf("fusion primary = %#v, want %s", values["COOP_PRIMARY"], lead.Provider)
	}
	wrapperCount := 0
	personaCount := make(map[string]int, len(members))
	credentialCount := make(map[string]int, len(suite.providers))
	for _, mount := range event.Run.Mounts {
		if mount.Target == "<container>"+fusion.ConsultWrapperPath && mount.ReadOnly {
			wrapperCount++
		}
		for _, member := range members {
			if mount.Target == "<container>/home/node/.coop/consult/"+member+".md" && mount.ReadOnly {
				personaCount[member]++
			}
		}
		for _, provider := range suite.providers {
			want := processTracePath(suite.layout.Root, filepath.Join(suite.layout.Config, provider, "profiles", "personal"))
			if mount.Source == want && mount.Target == "<container>/home/node/."+provider && !mount.ReadOnly {
				credentialCount[provider]++
			}
		}
	}
	if wrapperCount != 1 {
		t.Errorf("fusion consult wrapper mounts = %d, want 1", wrapperCount)
	}
	for _, provider := range suite.providers {
		role := compositionRole(provider)
		key := strings.ToUpper(strings.ReplaceAll(role, "-", "_"))
		if values["COOP_CONSULT_"+key+"_TARGETS"].Value != roles[provider].String() {
			t.Errorf("%s target env = %#v, want %s", role, values["COOP_CONSULT_"+key+"_TARGETS"], roles[provider].String())
		}
		if personaCount[role] != 1 {
			t.Errorf("%s persona mounts = %d, want 1", role, personaCount[role])
		}
		if credentialCount[provider] != 1 {
			t.Errorf("%s credential mounts = %d, want 1", provider, credentialCount[provider])
		}
	}
	if leadRole := roles[lead.Provider]; leadRole.Model == lead.Model || leadRole.Effort != lead.Effort {
		t.Errorf("same-provider lead/role targets did not preserve distinct models with the same effort: lead=%s role=%s", lead.String(), leadRole.String())
	}
}
