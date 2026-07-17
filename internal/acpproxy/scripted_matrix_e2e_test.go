package acpproxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

type matrixPlan struct {
	Providers map[string][][]matrixStep `json:"providers"`
}

type matrixStep struct {
	Method        string `json:"method"`
	Params        any    `json:"params,omitempty"`
	Result        any    `json:"result,omitempty"`
	Error         any    `json:"error,omitempty"`
	Events        []any  `json:"events,omitempty"`
	EchoPrompt    bool   `json:"echo_prompt,omitempty"`
	DeferResponse bool   `json:"defer_response,omitempty"`
}

type matrixProvider struct {
	name   string
	prefix string
}

var matrixProviders = []matrixProvider{
	{name: "claude", prefix: "CL"},
	{name: "codex", prefix: "CO"},
	{name: "gemini", prefix: "GE"},
	{name: "grok", prefix: "GR"},
}

func TestScriptedACPProviderOracleCoversRegistry(t *testing.T) {
	names := make([]string, 0, len(matrixProviders))
	for _, provider := range matrixProviders {
		names = append(names, provider.name)
	}
	if want := agents.Names(); !slices.Equal(names, want) {
		t.Fatalf("scripted ACP providers = %v, registered providers = %v", names, want)
	}
}

func TestScriptedACPDirectedProviderSwitchMatrix(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	coopBin := filepath.Join(buildDir, "coop")
	fixtureBin := filepath.Join(buildDir, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")

	for _, source := range matrixProviders {
		for _, destination := range matrixProviders {
			if source.name == destination.name {
				continue
			}
			t.Run(source.name+"_to_"+destination.name, func(t *testing.T) {
				tmp := t.TempDir()
				repo := filepath.Join(tmp, "repo")
				if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
					t.Fatal(err)
				}
				sourceID, destinationID := source.prefix+"1", destination.prefix+"2"
				planPath := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
					source.name:      {matrixGeneration(source.name, sourceID, repo, "source answer")},
					destination.name: {matrixGeneration(destination.name, destinationID, repo, "destination answer")},
				}})
				proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, source.name, source.name, destination.name)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()

				initialize, err := proc.client.req(ctx, "initialize", map[string]any{
					"protocolVersion": 1, "clientCapabilities": map[string]any{},
				})
				if err != nil {
					t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
				}
				assertEditorAuthenticationHidden(t, initialize)
				response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
				if err != nil {
					t.Fatalf("session/new: %v\nstderr:\n%s", err, proc.stderr.String())
				}
				editorID := responseSessionID(response)
				if editorID != sourceID {
					t.Fatalf("editor session id = %q, want source native id %q", editorID, sourceID)
				}
				promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", source.name+"-0", "wire.jsonl"), editorID, "source question", "source answer")
				switchScriptedProvider(t, ctx, proc, editorID, destination.name)
				promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", destination.name+"-0", "wire.jsonl"), editorID, "destination question", "destination answer")

				state := filepath.Join(tmp, "fixture-state")
				assertMatrixChild(t, state, source.name, sourceID, repo, false)
				assertMatrixChild(t, state, destination.name, destinationID, repo, true)
				target, err := os.ReadFile(filepath.Join(state, destination.name+"-0", "target.txt"))
				if err != nil {
					t.Fatal(err)
				}
				if got, want := string(target), destination.name+"@default"; got != want {
					t.Fatalf("destination target = %q, want %q", got, want)
				}
				for _, frame := range proc.client.transcript() {
					if frame.Msg["method"] == "session/update" && strings.Contains(string(frame.Raw), "[coop] This thread continues") {
						t.Fatalf("synthetic carry leaked to editor: %s", frame.Raw)
					}
				}
			})
		}
	}
}

func TestScriptedACPProviderRateLimitAutoRecovery(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	coopBin := filepath.Join(buildDir, "coop")
	fixtureBin := filepath.Join(buildDir, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")

	limitErrors := map[string]any{
		"claude": map[string]any{"code": -32603, "message": "provider declined", "data": map[string]any{"errorKind": "rate_limit"}},
		"codex":  map[string]any{"code": -32603, "message": "Internal error", "data": map[string]any{"codexErrorInfo": "usageLimitExceeded"}},
		"gemini": map[string]any{"code": -32603, "message": "provider declined", "data": map[string]any{"code": "RESOURCE_EXHAUSTED"}},
		"grok":   map[string]any{"code": -32603, "message": "rate limit reached"},
	}
	for _, provider := range matrixProviders {
		t.Run(provider.name, func(t *testing.T) {
			tmp := t.TempDir()
			repo := filepath.Join(tmp, "repo")
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
				t.Fatal(err)
			}
			signInScriptedProfile(t, tmp, provider.name, "work")
			firstID, secondID := provider.prefix+"1", provider.prefix+"2"
			planPath := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
				provider.name: rateLimitGenerations(provider.name, firstID, secondID, repo, limitErrors[provider.name]),
			}})
			proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, provider.name, provider.name)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
				t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
			}
			response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
			if err != nil {
				t.Fatalf("session/new: %v\nstderr:\n%s", err, proc.stderr.String())
			}
			editorID := responseSessionID(response)
			mark := proc.client.mark()
			if _, err := proc.client.req(ctx, "session/prompt", map[string]any{
				"sessionId": editorID,
				"prompt":    []any{map[string]any{"type": "text", "text": "recover after limit"}},
			}); err != nil {
				t.Fatalf("automatic rate-limit retry: %v\nstderr:\n%s\nwire:\n%s", err, proc.stderr.String(), wireDump(proc.client.transcript()))
			}
			awaitScriptedEventContains(t, ctx, proc, mark, "recovered on work")

			state := filepath.Join(tmp, "fixture-state")
			for _, generation := range []string{provider.name + "-0", provider.name + "-1"} {
				assertFixtureHandshake(t, filepath.Join(state, generation, "wire.jsonl"))
			}
			target, err := os.ReadFile(filepath.Join(state, provider.name+"-1", "target.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(target), provider.name+"@work"; got != want {
				t.Fatalf("replacement target = %q, want %q", got, want)
			}
			for _, frame := range proc.client.transcript()[mark:] {
				raw := string(frame.Raw)
				if strings.Contains(raw, "raw rate-limit notice") || strings.Contains(raw, "usageLimitExceeded") || strings.Contains(raw, "RESOURCE_EXHAUSTED") || strings.Contains(raw, "rate_limit") {
					t.Fatalf("provider rate-limit UI leaked to editor: %s", frame.Raw)
				}
				if strings.Contains(raw, `"sessionUpdate":"config_option_update"`) && !strings.Contains(raw, `"currentValue":"auto"`) {
					t.Fatalf("automatic recovery pinned the account toolbar: %s", frame.Raw)
				}
			}
		})
	}
}

func TestScriptedACPProviderTargetReplayMatrix(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	coopBin := filepath.Join(buildDir, "coop")
	fixtureBin := filepath.Join(buildDir, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")

	for providerIndex, provider := range matrixProviders {
		t.Run(provider.name, func(t *testing.T) {
			const initialModel = "model-initial"
			const acceptedModel = "model-accepted"
			const rejectedModel = "model-rejected"
			tmp := t.TempDir()
			repo := filepath.Join(tmp, "repo")
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
				t.Fatal(err)
			}
			sessionID := provider.prefix + "1"
			bridge := matrixProviders[(providerIndex+1)%len(matrixProviders)]
			planPath := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
				provider.name: targetReplayGenerations(provider.name, sessionID, repo, initialModel, acceptedModel, rejectedModel),
				bridge.name:   {targetBridgeGeneration(bridge.name, bridge.prefix+"B")},
			}})
			target := provider.name + ":" + initialModel
			if provider.name != "gemini" {
				target += "/xhigh"
			}
			proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, target, provider.name, bridge.name)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
				t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
			}
			readyMark := proc.client.mark()
			response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
			if err != nil {
				t.Fatalf("session/new: %v\nstderr:\n%s", err, proc.stderr.String())
			}
			editorID := responseSessionID(response)
			if provider.name != "grok" {
				awaitScriptedEventContains(t, ctx, proc, readyMark, "target settings ready")
				if _, err := proc.client.req(ctx, "session/set_config_option", map[string]any{
					"sessionId": editorID, "configId": "model", "value": acceptedModel,
				}); err != nil {
					t.Fatalf("accept model change: %v\nstderr:\n%s", err, proc.stderr.String())
				}
			}
			promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", provider.name+"-0", "wire.jsonl"), editorID, "establish replay", "before reload")

			if provider.name != "grok" {
				_, err := proc.client.req(ctx, "session/set_config_option", map[string]any{
					"sessionId": editorID, "configId": "model", "value": rejectedModel,
				})
				if err == nil || !strings.Contains(err.Error(), "unsupported model") {
					t.Fatalf("rejected native model error = %v, want unsupported model", err)
				}
			}
			switchScriptedProvider(t, ctx, proc, editorID, bridge.name)
			switchScriptedProvider(t, ctx, proc, editorID, provider.name)
			promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", provider.name+"-1", "wire.jsonl"), editorID, "after switch back", "restored target after switch")

			mark := proc.client.mark()
			if err := proc.cmd.Process.Signal(syscall.SIGHUP); err != nil {
				t.Fatalf("SIGHUP scripted supervisor: %v", err)
			}
			awaitScriptedEventContains(t, ctx, proc, mark, `"sessionUpdate":"config_option_update"`)
			if provider.name != "grok" {
				awaitScriptedEventContains(t, ctx, proc, mark, "target settings ready")
			}
			promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", provider.name+"-2", "wire.jsonl"), editorID, "after reload", "replayed target")

			state := filepath.Join(tmp, "fixture-state")
			for _, generation := range []string{provider.name + "-0", provider.name + "-1", provider.name + "-2", bridge.name + "-0"} {
				assertFixtureHandshake(t, filepath.Join(state, generation, "wire.jsonl"))
			}
			replayWire, err := os.ReadFile(filepath.Join(state, provider.name+"-2", "wire.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			rejectedRequest := `"method":"session/set_config_option","params":{"configId":"model","sessionId":"` + sessionID + `","value":"` + rejectedModel + `"}`
			if provider.name == "gemini" {
				rejectedRequest = `"method":"session/set_model","params":{"modelId":"` + rejectedModel + `","sessionId":"` + sessionID + `"}`
			}
			if strings.Contains(string(replayWire), rejectedRequest) {
				t.Fatalf("rejected model poisoned replay:\n%s", replayWire)
			}
			for _, generation := range []string{provider.name + "-0", provider.name + "-1", provider.name + "-2"} {
				launchTarget, err := os.ReadFile(filepath.Join(state, generation, "target.txt"))
				if err != nil {
					t.Fatal(err)
				}
				wantModel := initialModel
				if provider.name != "grok" && generation != provider.name+"-0" {
					wantModel = acceptedModel
				}
				if !strings.Contains(string(launchTarget), ":"+wantModel) {
					t.Fatalf("%s launch target = %q, want model %q", generation, launchTarget, wantModel)
				}
			}
		})
	}
}

func TestScriptedACPGrokModelAgentMigration(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	coopBin := filepath.Join(tmp, "coop")
	fixtureBin := filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	const initialModel = "grok-4.5"
	const migratedModel = "grok-composer-2.5-fast"
	initialize := matrixStep{Method: "initialize", Result: map[string]any{
		"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{},
	}}
	models := func(current string) map[string]any {
		return targetModels(current, initialModel, migratedModel)
	}
	planPath := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
		"grok": {
			{
				initialize,
				{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Result: map[string]any{"sessionId": "GR1", "models": models(initialModel)}},
				{Method: "session/prompt", Events: []any{matrixAgentChunk("GR1", "old agent answered")}, Result: map[string]any{"stopReason": "end_turn"}},
				{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Result: map[string]any{"sessionId": "GRB1", "models": models(initialModel)}},
				{Method: "session/prompt", Events: []any{matrixAgentChunk("GRB1", "background seed answered")}, Result: map[string]any{"stopReason": "end_turn"}},
				{Method: "session/prompt", DeferResponse: true},
				{Method: "session/set_model", Params: map[string]any{"sessionId": "GR1", "modelId": migratedModel}, Error: map[string]any{
					"code": -32600, "message": "agent type mismatch", "data": map[string]any{
						"code": "MODEL_SWITCH_INCOMPATIBLE_AGENT", "suggestion": "start_new_session",
					},
				}},
			},
			{
				initialize,
				{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Result: map[string]any{"sessionId": "GR2", "models": models(migratedModel)}},
				{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Result: map[string]any{"sessionId": "GRB2", "models": models(migratedModel)}},
				{Method: "session/prompt", EchoPrompt: true, Events: []any{matrixAgentChunk("GRB2", "background migrated answer")}, Result: map[string]any{"stopReason": "end_turn"}},
				{Method: "session/prompt", EchoPrompt: true, Events: []any{matrixAgentChunk("GR2", "migrated agent answered")}, Result: map[string]any{"stopReason": "end_turn"}},
			},
			{
				initialize,
				{Method: "session/load", Params: map[string]any{"cwd": repo, "mcpServers": []any{}, "sessionId": "GR2"}, Result: map[string]any{"models": models(migratedModel)}},
				{Method: "session/load", Params: map[string]any{"cwd": repo, "mcpServers": []any{}, "sessionId": "GRB2"}, Result: map[string]any{"models": models(migratedModel)}},
				{Method: "session/prompt", Events: []any{matrixAgentChunk("GR2", "reloaded migrated agent answered")}, Result: map[string]any{"stopReason": "end_turn"}},
			},
		},
	}})
	proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "grok:"+initialModel+"/high", "grok")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	editorID := responseSessionID(response)
	promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", "grok-0", "wire.jsonl"), editorID, "before migration", "old agent answered")
	background, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("background session/new: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	backgroundID := responseSessionID(background)
	if backgroundID != "GRB1" {
		t.Fatalf("background editor id = %q, want GRB1", backgroundID)
	}
	promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", "grok-0", "wire.jsonl"), backgroundID, "background seed", "background seed answered")
	backgroundMark := proc.client.mark()
	backgroundDone := make(chan error, 1)
	go func() {
		_, err := proc.client.req(ctx, "session/prompt", map[string]any{
			"sessionId": backgroundID,
			"prompt":    []any{map[string]any{"type": "text", "text": "background in flight"}},
		})
		backgroundDone <- err
	}()
	awaitFixtureWireContains(t, ctx, filepath.Join(tmp, "fixture-state", "grok-0", "wire.jsonl"), "background in flight")

	migrationMark := proc.client.mark()
	options, err := proc.client.req(ctx, "session/set_config_option", map[string]any{
		"sessionId": editorID, "configId": "model", "value": migratedModel,
	})
	if err != nil {
		t.Fatalf("model migration: %v\nstderr:\n%s\nwire:\n%s", err, proc.stderr.String(), wireDump(proc.client.transcript()))
	}
	encodedOptions, _ := json.Marshal(options)
	if !strings.Contains(string(encodedOptions), `"id":"model"`) || !strings.Contains(string(encodedOptions), `"currentValue":"`+migratedModel+`"`) {
		t.Fatalf("migration ack does not select %q: %s", migratedModel, encodedOptions)
	}
	awaitScriptedEventContains(t, ctx, proc, migrationMark, `"sessionUpdate":"config_option_update"`)
	awaitScriptedEventContains(t, ctx, proc, backgroundMark, "background migrated answer")
	if err := <-backgroundDone; err != nil {
		t.Fatalf("background in-flight prompt did not complete transparently: %v\nstderr:\n%s\nwire:\n%s", err, proc.stderr.String(), wireDump(proc.client.transcript()))
	}
	promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", "grok-1", "wire.jsonl"), editorID, "after migration", "migrated agent answered")

	mark := proc.client.mark()
	if err := proc.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP scripted supervisor: %v", err)
	}
	awaitScriptedEventContains(t, ctx, proc, mark, `"sessionUpdate":"config_option_update"`)
	promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", "grok-2", "wire.jsonl"), editorID, "after reload", "reloaded migrated agent answered")

	state := filepath.Join(tmp, "fixture-state")
	migratedWire, err := os.ReadFile(filepath.Join(state, "grok-1", "wire.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(migratedWire), `"method":"session/new"`) != 2 || strings.Contains(string(migratedWire), `"method":"session/load"`) ||
		strings.Contains(string(migratedWire), `"sessionId":"GR1"`) || strings.Contains(string(migratedWire), `"sessionId":"GRB1"`) {
		t.Fatalf("model-agent migration did not create two fresh native sessions:\n%s", migratedWire)
	}
	carryRequests := 0
	for _, line := range strings.Split(string(migratedWire), "\n") {
		if strings.Contains(line, `"direction":"in"`) && strings.Contains(line, "[coop] This thread continues") {
			carryRequests++
		}
	}
	if carryRequests != 2 {
		t.Fatalf("migration carry request count = %d, want 2 (one per session):\n%s", carryRequests, migratedWire)
	}
	for _, want := range []string{`"sessionId":"GR2"`, `"sessionId":"GRB2"`, "background in flight", "background seed", "before migration"} {
		if !strings.Contains(string(migratedWire), want) {
			t.Fatalf("multi-session migration is missing %s:\n%s", want, migratedWire)
		}
	}
	for _, frame := range proc.client.transcript() {
		if frame.Msg["method"] == "session/update" && strings.Contains(string(frame.Raw), "[coop] This thread continues") {
			t.Fatalf("Grok prompt echo leaked synthetic carry to editor: %s", frame.Raw)
		}
		if strings.Contains(string(frame.Raw), "background migrated answer") && !strings.Contains(string(frame.Raw), `"sessionId":"GRB1"`) {
			t.Fatalf("background update lost stable editor identity: %s", frame.Raw)
		}
	}
	for _, generation := range []string{"grok-1", "grok-2"} {
		target, err := os.ReadFile(filepath.Join(state, generation, "target.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(target), ":"+migratedModel) {
			t.Fatalf("%s launch target = %q, want model %q", generation, target, migratedModel)
		}
	}
}

func awaitFixtureWireContains(t *testing.T, ctx context.Context, path, want string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), want) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("fixture wire %s did not contain %q before timeout", path, want)
		case <-ticker.C:
		}
	}
}

func TestScriptedACPProviderScopedCooldownSurvivesReload(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	coopBin := filepath.Join(tmp, "coop")
	fixtureBin := filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	signInScriptedProfile(t, tmp, "claude", "work")
	limit := map[string]any{"code": -32603, "message": "provider declined", "data": map[string]any{"errorKind": "rate_limit"}}
	codexReload := []matrixStep{
		{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
		{Method: "session/load", Params: map[string]any{"cwd": repo, "mcpServers": []any{}, "sessionId": "CO1"}, Result: map[string]any{"configOptions": []any{}}},
		{Method: "session/prompt", Params: map[string]any{"sessionId": "CO1", "prompt": []any{map[string]any{"type": "text", "text": "after scoped reload"}}}, Events: []any{matrixAgentChunk("CO1", "codex survived scoped reload")}, Result: map[string]any{"stopReason": "end_turn"}},
	}
	planPath := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
		"claude": rateLimitGenerations("claude", "CL1", "CL2", repo, limit),
		"codex":  {matrixGeneration("codex", "CO1", repo, "codex default available"), codexReload},
	}})
	proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "claude", "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize scoped cooldown: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new scoped cooldown: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	editorID := responseSessionID(response)
	mark := proc.client.mark()
	if _, err := proc.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": editorID,
		"prompt":    []any{map[string]any{"type": "text", "text": "recover after limit"}},
	}); err != nil {
		t.Fatalf("recover from Claude limit: %v\nstderr:\n%s\nwire:\n%s", err, proc.stderr.String(), wireDump(proc.client.transcript()))
	}
	awaitScriptedEventContains(t, ctx, proc, mark, "recovered on work")

	// Claude's default profile is now cooling. An unscoped cooldown map would make this Codex
	// switch wait for the same profile name until the test times out.
	switchScriptedProvider(t, ctx, proc, editorID, "codex")
	promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", "codex-0", "wire.jsonl"), editorID, "codex after claude limit", "codex default available")
	mark = proc.client.mark()
	if err := proc.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP scoped cooldown supervisor: %v", err)
	}
	awaitScriptedEventContains(t, ctx, proc, mark, `"sessionUpdate":"config_option_update"`)
	promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", "codex-1", "wire.jsonl"), editorID, "after scoped reload", "codex survived scoped reload")
	assertTargetFile(t, tmp, "claude-1", "claude@work")
	assertTargetFile(t, tmp, "codex-0", "codex@default")
	assertTargetFile(t, tmp, "codex-1", "codex@default")
}

func TestScriptedACPFrontierRateLimitAndReload(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	coopBin := filepath.Join(tmp, "coop")
	fixtureBin := filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	repo := filepath.Join(tmp, "repo")
	presetDir := filepath.Join(repo, ".agent", "presets", "frontier")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const preset = "lead:\n  agent: [claude:claude-fable-5/xhigh, codex:gpt-5.6-sol/xhigh]\n"
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte(preset), 0o600); err != nil {
		t.Fatal(err)
	}
	planPath := writeMatrixPlan(t, tmp, frontierRateLimitPlan(repo))
	proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "frontier", "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	readyMark := proc.client.mark()
	response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	sessionID := responseSessionID(response)
	awaitScriptedEventContains(t, ctx, proc, readyMark, "target settings ready")
	encoded, _ := json.Marshal(response)
	if !strings.Contains(string(encoded), `"id":"coop_preset"`) || !strings.Contains(string(encoded), `"currentValue":"frontier"`) ||
		!strings.Contains(string(encoded), "frontier · Claude Code · claude-fable-5") {
		t.Fatalf("Frontier preset is not active in the initial toolbar: %s", encoded)
	}
	for _, forbidden := range []string{`"id":"coop_provider"`, `"id":"coop_account"`, `"id":"model"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("preset leaked non-preset control %s: %s", forbidden, encoded)
		}
	}

	mark := proc.client.mark()
	if _, err := proc.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "continue on the frontier"}},
	}); err != nil {
		t.Fatalf("Frontier transparent retry: %v\nstderr:\n%s\nwire:\n%s", err, proc.stderr.String(), wireDump(proc.client.transcript()))
	}
	awaitScriptedEventContains(t, ctx, proc, mark, "recovered on Sol")
	assertTargetFile(t, tmp, "claude-0", "claude:claude-fable-5/xhigh@default")
	assertTargetFile(t, tmp, "codex-0", "codex:gpt-5.6-sol/xhigh@default")
	sawEffectiveSol := false
	for _, frame := range proc.client.transcript()[mark:] {
		sawEffectiveSol = sawEffectiveSol || strings.Contains(string(frame.Raw), "frontier · Codex · gpt-5.6-sol")
		if strings.Contains(string(frame.Raw), "reached your Fable 5 limit") || strings.Contains(string(frame.Raw), "errorKind") {
			t.Fatalf("Frontier rate-limit UI leaked to editor: %s", frame.Raw)
		}
	}
	if !sawEffectiveSol {
		t.Fatalf("Frontier selector never exposed the effective Codex/Sol rung:\n%s", wireDump(proc.client.transcript()[mark:]))
	}

	mark = proc.client.mark()
	if err := proc.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP scripted supervisor: %v", err)
	}
	awaitScriptedEventContains(t, ctx, proc, mark, "target settings ready")
	promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", "codex-1", "wire.jsonl"), sessionID, "after frontier reload", "Sol survived reload")
	assertTargetFile(t, tmp, "codex-1", "codex:gpt-5.6-sol/xhigh@default")
}

func TestScriptedACPPresetWalksEveryProvider(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	coopBin := filepath.Join(tmp, "coop")
	fixtureBin := filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	repo := filepath.Join(tmp, "repo")
	presetDir := filepath.Join(repo, ".agent", "presets", "provider-walk")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const preset = "lead:\n  agent: [claude:claude-fable-5/xhigh, codex:gpt-5.6-sol/xhigh, gemini:gemini-3.1-pro-preview, grok:grok-4.5/xhigh]\n"
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte(preset), 0o600); err != nil {
		t.Fatal(err)
	}
	planPath := writeMatrixPlan(t, tmp, presetProviderWalkPlan())
	proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "provider-walk", "claude", "codex", "gemini", "grok")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize provider walk: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	readyMark := proc.client.mark()
	response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new provider walk: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	sessionID := responseSessionID(response)
	awaitScriptedEventContains(t, ctx, proc, readyMark, "target settings ready")
	mark := proc.client.mark()
	if _, err := proc.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "walk every provider"}},
	}); err != nil {
		t.Fatalf("provider walk transparent retries: %v\nstderr:\n%s\nwire:\n%s", err, proc.stderr.String(), wireDump(proc.client.transcript()))
	}
	awaitScriptedEventContains(t, ctx, proc, mark, "provider walk complete")

	for _, tc := range []struct{ generation, target string }{
		{"claude-0", "claude:claude-fable-5/xhigh@default"},
		{"codex-0", "codex:gpt-5.6-sol/xhigh@default"},
		{"gemini-0", "gemini:gemini-3.1-pro-preview@default"},
		{"grok-0", "grok:grok-4.5/xhigh@default"},
	} {
		assertTargetFile(t, tmp, tc.generation, tc.target)
	}
	for _, frame := range proc.client.transcript()[mark:] {
		raw := string(frame.Raw)
		if strings.Contains(raw, "usageLimitExceeded") || strings.Contains(raw, "RESOURCE_EXHAUSTED") || strings.Contains(raw, "errorKind") {
			t.Fatalf("preset provider-walk limit leaked to editor: %s", frame.Raw)
		}
	}
}

func TestScriptedACPRateLimitDenials(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	coopBin := filepath.Join(buildDir, "coop")
	fixtureBin := filepath.Join(buildDir, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	cases := []struct {
		name      string
		error     any
		eventText string
		want      string
	}{
		{
			name:      "output token exhaustion",
			error:     map[string]any{"code": -32603, "message": "provider declined", "data": map[string]any{"stopReason": "MAX_TOKENS"}},
			eventText: "Output Limit Reached: maximum output length",
			want:      "MAX_TOKENS",
		},
		{
			name:  "foreign provider marker",
			error: map[string]any{"code": -32603, "message": "provider declined", "data": map[string]any{"codexErrorInfo": "usageLimitExceeded"}},
			want:  "usageLimitExceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			repo := filepath.Join(tmp, "repo")
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
				t.Fatal(err)
			}
			signInScriptedProfile(t, tmp, "claude", "work")
			steps := []matrixStep{
				{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
				{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Result: map[string]any{"sessionId": "CL1", "configOptions": []any{}}},
				{Method: "session/set_config_option", Params: map[string]any{"sessionId": "CL1", "configId": "mode", "value": "bypassPermissions"}, Result: map[string]any{"configOptions": []any{}}},
			}
			prompt := matrixStep{Method: "session/prompt", Error: tc.error}
			if tc.eventText != "" {
				prompt.Events = []any{matrixAgentChunk("CL1", tc.eventText)}
			}
			steps = append(steps, prompt)
			planPath := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{"claude": {steps}}})
			proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "claude", "claude")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
				t.Fatal(err)
			}
			response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
			if err != nil {
				t.Fatal(err)
			}
			mark := proc.client.mark()
			_, err = proc.client.req(ctx, "session/prompt", map[string]any{
				"sessionId": responseSessionID(response), "prompt": []any{map[string]any{"type": "text", "text": "do not rotate"}},
			})
			if err == nil {
				t.Fatal("denied rate-limit signal unexpectedly recovered")
			}
			visible := false
			for _, frame := range proc.client.transcript()[mark:] {
				visible = visible || strings.Contains(string(frame.Raw), tc.want) || (tc.eventText != "" && strings.Contains(string(frame.Raw), tc.eventText))
			}
			if !visible {
				t.Fatalf("denied signal %q was not visible to the editor:\n%s", tc.want, wireDump(proc.client.transcript()))
			}
			if _, err := os.Stat(filepath.Join(tmp, "fixture-state", "claude-1")); !os.IsNotExist(err) {
				t.Fatalf("denied signal restarted the provider: %v", err)
			}
		})
	}
}

func TestScriptedACPFailedDestinationSessionCanSwitchBack(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	coopBin := filepath.Join(tmp, "coop")
	fixtureBin := filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeFresh := matrixGeneration("claude", "CL1", repo, "Claude before failed switch")
	claudeReload := []matrixStep{
		{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
		{Method: "session/load", Params: map[string]any{"cwd": repo, "mcpServers": []any{}, "sessionId": "CL1"}, Result: map[string]any{"configOptions": []any{}}},
		{Method: "session/set_config_option", Params: map[string]any{"sessionId": "CL1", "configId": "mode", "value": "bypassPermissions"}, Result: map[string]any{"configOptions": []any{}}, Events: []any{matrixAgentChunk("CL1", "source restored")}},
		{Method: "session/prompt", Params: map[string]any{"sessionId": "CL1", "prompt": []any{map[string]any{"type": "text", "text": "continue after failed switch"}}}, Events: []any{matrixAgentChunk("CL1", "Claude after failed switch")}, Result: map[string]any{"stopReason": "end_turn"}},
	}
	codexFailed := []matrixStep{
		{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
		{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Error: map[string]any{"code": -32000, "message": "destination session creation failed"}},
	}
	planPath := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
		"claude": {claudeFresh, claudeReload},
		"codex":  {codexFailed},
	}})
	proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, planPath, "claude", "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := responseSessionID(response)
	promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", "claude-0", "wire.jsonl"), sessionID, "before failed switch", "Claude before failed switch")

	mark := proc.client.mark()
	if _, err := proc.client.req(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID, "configId": "coop_provider", "value": "codex",
	}); err != nil {
		t.Fatalf("switch to failing destination: %v", err)
	}
	awaitScriptedEventContains(t, ctx, proc, mark, "could not be re-established")
	codexWire, err := os.ReadFile(filepath.Join(tmp, "fixture-state", "codex-0", "wire.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(codexWire), `"method":"session/prompt"`) || strings.Contains(string(codexWire), `"method":"session/load"`) {
		t.Fatalf("failed destination received a phantom session binding:\n%s", codexWire)
	}

	if _, err := proc.client.req(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID, "configId": "coop_provider", "value": "claude",
	}); err != nil {
		t.Fatalf("switch back to source: %v", err)
	}
	promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", "claude-1", "wire.jsonl"), sessionID, "continue after failed switch", "Claude after failed switch")
}

func matrixGeneration(provider, sessionID, repo, answer string) []matrixStep {
	steps := []matrixStep{
		{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
		{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Result: map[string]any{"sessionId": sessionID, "configOptions": []any{}}},
	}
	if provider == "claude" {
		steps = append(steps, matrixStep{
			Method: "session/set_config_option",
			Params: map[string]any{"sessionId": sessionID, "configId": "mode", "value": "bypassPermissions"},
			Result: map[string]any{"configOptions": []any{}},
		})
	}
	return append(steps, matrixStep{
		Method: "session/prompt",
		Events: []any{map[string]any{
			"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]any{"sessionId": sessionID, "update": map[string]any{
				"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": answer},
			}},
		}},
		Result: map[string]any{"stopReason": "end_turn"},
	})
}

func rateLimitGenerations(provider, firstID, secondID, repo string, limitError any) [][]matrixStep {
	first := []matrixStep{
		{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
		{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Result: map[string]any{"sessionId": firstID, "configOptions": []any{}}},
	}
	second := []matrixStep{
		{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
		{Method: "session/new", Result: map[string]any{"sessionId": secondID, "configOptions": []any{}}},
	}
	if provider == "claude" {
		first = append(first, matrixStep{Method: "session/set_config_option", Params: map[string]any{
			"sessionId": firstID, "configId": "mode", "value": "bypassPermissions",
		}, Result: map[string]any{"configOptions": []any{}}})
		second = append(second, matrixStep{Method: "session/set_config_option", Params: map[string]any{
			"sessionId": secondID, "configId": "mode", "value": "bypassPermissions",
		}, Result: map[string]any{"configOptions": []any{}}})
	}
	first = append(first, matrixStep{
		Method: "session/prompt",
		Events: []any{map[string]any{
			"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]any{"sessionId": firstID, "update": map[string]any{
				"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "raw rate-limit notice"},
			}},
		}},
		Error: limitError,
	})
	second = append(second, matrixStep{
		Method: "session/prompt",
		Params: map[string]any{
			"sessionId": secondID,
			"prompt":    []any{map[string]any{"type": "text", "text": "recover after limit"}},
		},
		Events: []any{map[string]any{
			"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]any{"sessionId": secondID, "update": map[string]any{
				"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "recovered on work"},
			}},
		}},
		Result: map[string]any{"stopReason": "end_turn"},
	})
	return [][]matrixStep{first, second}
}

func targetReplayGenerations(provider, sessionID, repo, initialModel, acceptedModel, rejectedModel string) [][]matrixStep {
	initialize := matrixStep{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}}
	returnID := sessionID + "R"
	newResult := map[string]any{"sessionId": sessionID, "configOptions": targetConfigOptions(provider, initialModel)}
	returnResult := map[string]any{"sessionId": returnID, "configOptions": targetConfigOptions(provider, acceptedModel)}
	loadResult := map[string]any{"configOptions": targetConfigOptions(provider, acceptedModel)}
	if provider == "gemini" {
		models := targetModels(initialModel, acceptedModel, rejectedModel)
		newResult = map[string]any{"sessionId": sessionID, "models": models}
		returnResult = map[string]any{"sessionId": returnID, "models": models}
		loadResult = map[string]any{"models": models}
	}
	first := []matrixStep{
		initialize,
		{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Result: newResult},
	}
	first = append(first, targetSettings(provider, sessionID, initialModel)...)
	if provider != "grok" {
		method, params := targetModelRequest(provider, sessionID, acceptedModel)
		first = append(first, matrixStep{Method: method, Params: params, Result: map[string]any{"configOptions": targetConfigOptions(provider, acceptedModel)}})
	}
	first = append(first, matrixStep{
		Method: "session/prompt",
		Params: map[string]any{"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": "establish replay"}}},
		Events: []any{matrixAgentChunk(sessionID, "before reload")},
		Result: map[string]any{"stopReason": "end_turn"},
	})
	if provider != "grok" {
		method, params := targetModelRequest(provider, sessionID, rejectedModel)
		first = append(first, matrixStep{Method: method, Params: params, Error: map[string]any{"code": -32602, "message": "unsupported model"}})
	}

	replayModel := acceptedModel
	if provider == "grok" {
		replayModel = initialModel
		returnResult = map[string]any{"sessionId": returnID, "configOptions": targetConfigOptions(provider, initialModel)}
		loadResult = map[string]any{"configOptions": targetConfigOptions(provider, initialModel)}
	}
	second := []matrixStep{
		initialize,
		{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Result: returnResult},
	}
	second = append(second, targetSettings(provider, returnID, replayModel)...)
	second = append(second, matrixStep{
		Method: "session/prompt",
		Events: []any{matrixAgentChunk(returnID, "restored target after switch")},
		Result: map[string]any{"stopReason": "end_turn"},
	})
	third := []matrixStep{
		initialize,
		{Method: "session/load", Params: map[string]any{"cwd": repo, "mcpServers": []any{}, "sessionId": returnID}, Result: loadResult},
	}
	third = append(third, targetSettings(provider, returnID, replayModel)...)
	third = append(third, matrixStep{
		Method: "session/prompt",
		Params: map[string]any{"sessionId": returnID, "prompt": []any{map[string]any{"type": "text", "text": "after reload"}}},
		Events: []any{matrixAgentChunk(returnID, "replayed target")},
		Result: map[string]any{"stopReason": "end_turn"},
	})
	return [][]matrixStep{first, second, third}
}

func targetBridgeGeneration(provider, sessionID string) []matrixStep {
	steps := []matrixStep{
		{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
		{Method: "session/new", Result: map[string]any{"sessionId": sessionID, "configOptions": []any{}}},
	}
	if provider == "claude" {
		steps = append(steps, matrixStep{Method: "session/set_config_option", Result: map[string]any{"configOptions": []any{}}})
	}
	return steps
}

func targetSettings(provider, sessionID, model string) []matrixStep {
	setting := func(method string, params map[string]any) matrixStep {
		return matrixStep{Method: method, Params: params, Result: map[string]any{"configOptions": targetConfigOptions(provider, model)}}
	}
	var settings []matrixStep
	switch provider {
	case "claude":
		settings = []matrixStep{
			setting("session/set_config_option", map[string]any{"sessionId": sessionID, "configId": "mode", "value": "bypassPermissions"}),
			setting("session/set_config_option", map[string]any{"sessionId": sessionID, "configId": "model", "value": model}),
			setting("session/set_config_option", map[string]any{"sessionId": sessionID, "configId": "effort", "value": "xhigh"}),
		}
	case "codex":
		settings = []matrixStep{
			setting("session/set_config_option", map[string]any{"sessionId": sessionID, "configId": "model", "value": model}),
			setting("session/set_config_option", map[string]any{"sessionId": sessionID, "configId": "reasoning_effort", "value": "xhigh"}),
		}
	case "gemini":
		settings = []matrixStep{setting("session/set_model", map[string]any{"sessionId": sessionID, "modelId": model})}
	default:
		return nil
	}
	settings[len(settings)-1].Events = []any{matrixAgentChunk(sessionID, "target settings ready")}
	return settings
}

func targetModelRequest(provider, sessionID, model string) (string, map[string]any) {
	if provider == "gemini" {
		return "session/set_model", map[string]any{"sessionId": sessionID, "modelId": model}
	}
	return "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": "model", "value": model}
}

func targetConfigOptions(provider, currentModel string) []any {
	if provider == "grok" || provider == "gemini" {
		return []any{}
	}
	effortID := "effort"
	if provider == "codex" {
		effortID = "reasoning_effort"
	}
	return []any{
		map[string]any{"id": "model", "type": "select", "currentValue": currentModel, "options": []any{}},
		map[string]any{"id": effortID, "type": "select", "currentValue": "xhigh", "options": []any{}},
	}
}

func targetModels(currentModel string, models ...string) map[string]any {
	available := make([]any, 0, len(models))
	for _, model := range models {
		available = append(available, map[string]any{"modelId": model, "name": model})
	}
	return map[string]any{"currentModelId": currentModel, "availableModels": available}
}

func matrixAgentChunk(sessionID, text string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": sessionID, "update": map[string]any{
			"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text},
		}},
	}
}

func presetProviderWalkPlan() matrixPlan {
	type rung struct {
		provider, sessionID, model string
		limit                      any
	}
	rungs := []rung{
		{provider: "claude", sessionID: "CL1", model: "claude-fable-5", limit: map[string]any{"code": -32603, "message": "rate limited", "data": map[string]any{"errorKind": "rate_limit"}}},
		{provider: "codex", sessionID: "CO2", model: "gpt-5.6-sol", limit: map[string]any{"code": -32603, "message": "rate limited", "data": map[string]any{"codexErrorInfo": "usageLimitExceeded"}}},
		{provider: "gemini", sessionID: "GE3", model: "gemini-3.1-pro-preview", limit: map[string]any{"code": -32603, "message": "rate limited", "data": map[string]any{"status": "RESOURCE_EXHAUSTED"}}},
		{provider: "grok", sessionID: "GR4", model: "grok-4.5"},
	}
	providers := make(map[string][][]matrixStep, len(rungs))
	for _, rung := range rungs {
		steps := []matrixStep{
			{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
			{Method: "session/new", Result: map[string]any{"sessionId": rung.sessionID, "configOptions": targetConfigOptions(rung.provider, rung.model)}},
		}
		steps = append(steps, targetSettings(rung.provider, rung.sessionID, rung.model)...)
		prompt := matrixStep{Method: "session/prompt"}
		if rung.limit != nil {
			prompt.Error = rung.limit
		} else {
			prompt.Events = []any{matrixAgentChunk(rung.sessionID, "provider walk complete")}
			prompt.Result = map[string]any{"stopReason": "end_turn"}
		}
		steps = append(steps, prompt)
		providers[rung.provider] = [][]matrixStep{steps}
	}
	return matrixPlan{Providers: providers}
}

func frontierRateLimitPlan(repo string) matrixPlan {
	claudeID, codexID := "CL1", "CO2"
	claude := []matrixStep{
		{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
		{Method: "session/new", Params: map[string]any{"cwd": repo, "mcpServers": []any{}}, Result: map[string]any{"sessionId": claudeID, "configOptions": targetConfigOptions("claude", "claude-fable-5")}},
	}
	claude = append(claude, targetSettings("claude", claudeID, "claude-fable-5")...)
	claude = append(claude, matrixStep{
		Method: "session/prompt",
		Events: []any{matrixAgentChunk(claudeID, "You've reached your Fable 5 limit.")},
		Error:  map[string]any{"code": -32603, "message": "You've reached your Fable 5 limit.", "data": map[string]any{"errorKind": "rate_limit"}},
	})

	codexFresh := []matrixStep{
		{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
		{Method: "session/new", Result: map[string]any{"sessionId": codexID, "configOptions": targetConfigOptions("codex", "gpt-5.6-sol")}},
	}
	codexFresh = append(codexFresh, targetSettings("codex", codexID, "gpt-5.6-sol")...)
	codexFresh = append(codexFresh, matrixStep{
		Method: "session/prompt",
		Events: []any{matrixAgentChunk(codexID, "recovered on Sol")},
		Result: map[string]any{"stopReason": "end_turn"},
	})

	codexReload := []matrixStep{
		{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}},
		{Method: "session/load", Params: map[string]any{"cwd": repo, "mcpServers": []any{}, "sessionId": codexID}, Result: map[string]any{"configOptions": targetConfigOptions("codex", "gpt-5.6-sol")}},
	}
	codexReload = append(codexReload, targetSettings("codex", codexID, "gpt-5.6-sol")...)
	codexReload = append(codexReload, matrixStep{
		Method: "session/prompt",
		Params: map[string]any{"sessionId": codexID, "prompt": []any{map[string]any{"type": "text", "text": "after frontier reload"}}},
		Events: []any{matrixAgentChunk(codexID, "Sol survived reload")},
		Result: map[string]any{"stopReason": "end_turn"},
	})
	return matrixPlan{Providers: map[string][][]matrixStep{"claude": {claude}, "codex": {codexFresh, codexReload}}}
}

func assertTargetFile(t *testing.T, tmp, generation, want string) {
	t.Helper()
	target, err := os.ReadFile(filepath.Join(tmp, "fixture-state", generation, "target.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(target); got != want {
		t.Fatalf("%s target = %q, want %q", generation, got, want)
	}
}

func writeMatrixPlan(t *testing.T, dir string, plan matrixPlan) string {
	t.Helper()
	// Events are emitted as raw ACP frames, so keep each encoded object on one line.
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func promptMatrix(t *testing.T, ctx context.Context, proc *scriptedACP, fixtureWire, sid, prompt, answer string) {
	t.Helper()
	mark := proc.client.mark()
	if _, err := proc.client.req(ctx, "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": prompt}},
	}); err != nil {
		t.Fatalf("session/prompt %q: %v\nstderr:\n%s\nwire:\n%s", prompt, err, proc.stderr.String(), wireDump(proc.client.transcript()))
	}
	awaitScriptedEventContains(t, ctx, proc, mark, answer)
}

func assertMatrixChild(t *testing.T, state, provider, nativeID, repo string, carried bool) {
	t.Helper()
	path := filepath.Join(state, provider+"-0", "wire.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(data)
	assertFixtureHandshake(t, path)
	if strings.Contains(wire, `"method":"session/load"`) {
		t.Fatalf("%s received a foreign session/load:\n%s", provider, wire)
	}
	for _, want := range []string{
		`"method":"session/new"`,
		fmt.Sprintf(`"cwd":%q`, repo),
		fmt.Sprintf(`"sessionId":%q`, nativeID),
	} {
		if !strings.Contains(wire, want) {
			t.Fatalf("%s wire is missing %s:\n%s", provider, want, wire)
		}
	}
	carryCount := strings.Count(wire, "[coop] This thread continues")
	if carried && carryCount != 1 {
		t.Fatalf("%s carry wrappers = %d, want one:\n%s", provider, carryCount, wire)
	}
	if !carried && carryCount != 0 {
		t.Fatalf("source %s unexpectedly received carry:\n%s", provider, wire)
	}
}
