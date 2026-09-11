package acpproxy_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestScriptedACPAutomaticStartup(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	coopBin, fixtureBin := filepath.Join(buildDir, "coop"), filepath.Join(buildDir, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	for _, tc := range []struct {
		name      string
		providers []string
		lead      string
		switchTo  string
	}{
		{name: "one provider", providers: []string{"codex"}, lead: "codex"},
		{name: "all providers", providers: []string{"grok", "gemini", "codex", "claude"}, lead: "claude", switchTo: "codex"},
		{name: "earlier provider unsigned", providers: []string{"grok", "gemini"}, lead: "gemini", switchTo: "grok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			repo := filepath.Join(tmp, "repo")
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
				t.Fatal(err)
			}
			plan := matrixPlan{Providers: map[string][][]matrixStep{
				tc.lead: {matrixGeneration(tc.lead, "initial", repo, "automatic answer")},
			}}
			if tc.switchTo != "" {
				reload := matrixGeneration(tc.switchTo, "switched", repo, "reloaded answer")
				reload[1] = matrixStep{Method: "session/load", Params: map[string]any{
					"cwd": repo, "mcpServers": []any{}, "sessionId": "switched",
				}, Result: map[string]any{"configOptions": []any{}}}
				plan.Providers[tc.switchTo] = [][]matrixStep{
					matrixGeneration(tc.switchTo, "switched", repo, "switched answer"), reload,
				}
			}
			proc := startScriptedACP(t, coopBin, fixtureBin, repo, tmp, writeMatrixPlan(t, tmp, plan), "", tc.providers...)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
				t.Fatalf("automatic initialize: %v\nstderr:\n%s", err, proc.stderr.String())
			}
			response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
			if err != nil {
				t.Fatalf("automatic session/new: %v\nstderr:\n%s", err, proc.stderr.String())
			}
			sid := responseSessionID(response)
			state := filepath.Join(tmp, "fixture-state")
			promptMatrix(t, ctx, proc, filepath.Join(state, tc.lead+"-0", "wire.jsonl"), sid, "first question", "automatic answer")
			target, err := os.ReadFile(filepath.Join(state, tc.lead+"-0", "target.txt"))
			if err != nil || string(target) != tc.lead+"@default" {
				t.Fatalf("automatic runtime target = %q, error=%v", target, err)
			}
			if tc.switchTo == "" {
				return
			}
			switchScriptedProvider(t, ctx, proc, sid, tc.switchTo)
			promptMatrix(t, ctx, proc, filepath.Join(state, tc.switchTo+"-0", "wire.jsonl"), sid, "next question", "switched answer")
			mark := proc.client.mark()
			if err := proc.cmd.Process.Signal(syscall.SIGHUP); err != nil {
				t.Fatal(err)
			}
			awaitScriptedEventContains(t, ctx, proc, mark, `"sessionUpdate":"config_option_update"`)
			promptMatrix(t, ctx, proc, filepath.Join(state, tc.switchTo+"-1", "wire.jsonl"), sid, "after reload", "reloaded answer")
			target, err = os.ReadFile(filepath.Join(state, tc.switchTo+"-1", "target.txt"))
			if err != nil || string(target) != tc.switchTo+"@default" {
				t.Fatalf("reload lost selected provider: target=%q, error=%v", target, err)
			}
		})
	}
}
