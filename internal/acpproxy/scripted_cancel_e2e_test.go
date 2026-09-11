package acpproxy_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScriptedACPCancelAndContinue(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	coopBin, fixtureBin := filepath.Join(buildDir, "coop"), filepath.Join(buildDir, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	for _, provider := range matrixProviders {
		t.Run(provider.name, func(t *testing.T) {
			tmp := t.TempDir()
			repo := filepath.Join(tmp, "repo")
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
				t.Fatal(err)
			}
			sessionID := provider.prefix + "1"
			steps := matrixGeneration(provider.name, sessionID, repo, "after cancel")
			steps = append(steps[:len(steps)-1],
				matrixStep{Method: "session/prompt", DeferResponse: true, Events: []any{matrixAgentChunk(sessionID, "in flight")}},
				matrixStep{Method: "session/cancel", Params: map[string]any{"sessionId": sessionID}, CompleteDeferred: true, Result: map[string]any{"stopReason": "cancelled"}},
				matrixStep{Method: "session/prompt", Events: []any{matrixAgentChunk(sessionID, "after cancel")}, Result: map[string]any{"stopReason": "end_turn"}},
			)
			plan := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{provider.name: {steps}}})
			proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, plan, provider.name, provider.name)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
				t.Fatal(err)
			}
			response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
			if err != nil {
				t.Fatal(err)
			}
			editorID := responseSessionID(response)
			mark := proc.client.mark()
			type result struct {
				response map[string]any
				err      error
			}
			done := make(chan result, 1)
			go func() {
				response, err := proc.client.req(ctx, "session/prompt", map[string]any{
					"sessionId": editorID, "prompt": []any{map[string]any{"type": "text", "text": "cancel this turn"}},
				})
				done <- result{response, err}
			}()
			awaitScriptedEventContains(t, ctx, proc, mark, "in flight")
			if err := proc.client.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": editorID}}); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("cancelled prompt: %v\nstderr:\n%s", got.err, proc.stderr.String())
				}
				body, _ := got.response["result"].(map[string]any)
				if body["stopReason"] != "cancelled" {
					t.Fatalf("cancelled prompt result: %v", body)
				}
			case <-ctx.Done():
				t.Fatalf("cancel did not complete prompt: %v\nstderr:\n%s", ctx.Err(), proc.stderr.String())
			}
			promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", provider.name+"-0", "wire.jsonl"), editorID, "continue", "after cancel")
		})
	}
}

func TestScriptedACPCancelQuotaWaitThenSwitch(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	coopBin, fixtureBin := filepath.Join(tmp, "coop"), filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	claude := matrixGeneration("claude", "CL1", repo, "unused")
	claude[len(claude)-1] = matrixStep{Method: "session/prompt", Error: map[string]any{
		"code": -32603, "message": "quota exhausted", "data": map[string]any{"errorKind": "rate_limit"},
	}}
	plan := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
		"claude": {claude}, "codex": {matrixGeneration("codex", "CO1", repo, "after quota cancellation")},
	}})
	proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, plan, "", "claude", "codex")
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
	mark := proc.client.mark()
	type result struct {
		response map[string]any
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := proc.client.req(ctx, "session/prompt", map[string]any{
			"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": "cancel this limited prompt"}},
		})
		done <- result{response, err}
	}()
	awaitScriptedEventContains(t, ctx, proc, mark, "Waiting for account")
	if err := proc.client.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sessionID}}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		body, _ := got.response["result"].(map[string]any)
		if got.err != nil || body["stopReason"] != "cancelled" {
			t.Fatalf("quota-wait cancellation = %v, %v\nstderr:\n%s", body, got.err, proc.stderr.String())
		}
	case <-ctx.Done():
		t.Fatalf("quota-wait cancellation hung: %v\nstderr:\n%s", ctx.Err(), proc.stderr.String())
	}
	switchScriptedProvider(t, ctx, proc, sessionID, "codex")
	promptMatrix(t, ctx, proc, filepath.Join(tmp, "fixture-state", "codex-0", "wire.jsonl"), sessionID, "new prompt", "after quota cancellation")
	if _, err := os.Stat(filepath.Join(tmp, "fixture-state", "claude-1")); !os.IsNotExist(err) {
		t.Fatalf("obsolete quota wait launched another Claude generation: %v", err)
	}
}
