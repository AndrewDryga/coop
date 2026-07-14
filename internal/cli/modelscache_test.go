package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/config"
)

// TestModelsCacheRoundTrip: a written cache reads back live; an empty write is a no-op that
// never clobbers a good cache; a cache older than the TTL reads as not-live (→ static).
func TestModelsCacheRoundTrip(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	if _, ok := loadModelsCache(cfg, "claude"); ok {
		t.Fatal("cold cache must not read as live")
	}
	want := []modelInfo{{ID: "opus", Name: "Opus"}, {ID: "sonnet", Name: "Sonnet"}}
	if err := writeModelsCache(cfg, "claude", want); err != nil {
		t.Fatal(err)
	}
	got, ok := loadModelsCache(cfg, "claude")
	if !ok || len(got.Models) != 2 || got.Models[0].ID != "opus" || got.Models[1].ID != "sonnet" {
		t.Fatalf("warm cache = (%v, %v), want the two written models live", got, ok)
	}
	if got.FetchedAt.IsZero() {
		t.Error("a live cache should carry its FetchedAt for the Last-refreshed line")
	}
	// An empty fetch is a no-op — it must not wipe the good cache.
	if err := writeModelsCache(cfg, "claude", nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := loadModelsCache(cfg, "claude"); !ok || len(got.Models) != 2 {
		t.Fatalf("empty write clobbered the cache: (%v, %v)", got, ok)
	}
}

// TestModelsCacheExpiry: a cache stamped past the TTL is not "live" — coop falls back to
// static — but its FetchedAt survives so the menu can say how stale the last fetch is.
func TestModelsCacheExpiry(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	stale := modelsCache{
		Models:    []modelInfo{{ID: "opus"}},
		FetchedAt: time.Now().Add(-modelsCacheTTL - time.Hour),
	}
	b, _ := json.Marshal(stale)
	dir := filepath.Join(cfg.ConfigDir, "claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelsCachePath(cfg, "claude"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	mc, ok := loadModelsCache(cfg, "claude")
	if ok {
		t.Error("an expired cache must not read as live")
	}
	if mc.FetchedAt.IsZero() {
		t.Error("an expired cache should keep FetchedAt for the stale note")
	}
	// And the menu says so: static examples with a "stale" Last-refreshed line.
	out := captureStdout(t, func() { (&app{cfg: cfg}).cmdModels([]string{"claude"}) })
	if !strings.Contains(out, "— stale") || !strings.Contains(out, "claude-sonnet-5") {
		t.Errorf("an expired cache should show the static list with a stale note:\n%s", out)
	}
}

// TestParseGrokModels: `grok models` bullets → ids, the default's " (default)" marker stripped,
// dupes and non-bullet lines ignored.
func TestParseGrokModels(t *testing.T) {
	out := `Available models:
  * grok-4.5 (default)
  - grok-composer-2.5-fast
  - grok-4.5

not a bullet line
`
	got := parseGrokModels([]byte(out))
	want := []string{"grok-4.5", "grok-composer-2.5-fast"}
	if len(got) != len(want) {
		t.Fatalf("parseGrokModels = %v, want ids %v", got, want)
	}
	for i, id := range want {
		if got[i].ID != id || got[i].Name != id {
			t.Errorf("model %d = %+v, want id/name %q", i, got[i], id)
		}
	}
}

// TestParseCodexModels: only visibility=="list" models survive, slug + display_name captured,
// a missing display_name falls back to the slug.
func TestParseCodexModels(t *testing.T) {
	out := `{"models":[
	  {"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list"},
	  {"slug":"gpt-5.4","display_name":"","visibility":"list"},
	  {"slug":"internal-only","display_name":"Hidden","visibility":"hidden"},
	  {"slug":"","display_name":"Blank","visibility":"list"}
	]}`
	got, err := parseCodexModels([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("parseCodexModels = %v, want only the two list-visible, non-blank models", got)
	}
	if got[0].ID != "gpt-5.5" || got[0].Name != "GPT-5.5" {
		t.Errorf("model 0 = %+v", got[0])
	}
	if got[1].ID != "gpt-5.4" || got[1].Name != "gpt-5.4" {
		t.Errorf("model 1 = %+v, want display_name to fall back to the slug", got[1])
	}
	if _, err := parseCodexModels([]byte("not json")); err == nil {
		t.Error("malformed JSON must error (→ caller keeps static)")
	}
}

// TestParseGeminiModels: the ACP session/new `models` field → cache entries, blank ids skipped.
func TestParseGeminiModels(t *testing.T) {
	models := json.RawMessage(`{"currentModelId":"gemini-3.5-flash","availableModels":[
	  {"modelId":"gemini-3.5-flash","name":"Flash"},
	  {"modelId":"gemini-2.5-pro","name":"Pro"},
	  {"modelId":"","name":"Blank"}
	]}`)
	got := parseGeminiModels(models)
	if len(got) != 2 || got[0].ID != "gemini-3.5-flash" || got[0].Name != "Flash" || got[1].ID != "gemini-2.5-pro" {
		t.Fatalf("parseGeminiModels = %v", got)
	}
	if parseGeminiModels(nil) != nil {
		t.Error("absent models field → nil")
	}
}

// TestParseClaudeModelOption: the claude configOptions `model` select → cache entries, blank
// values skipped.
func TestParseClaudeModelOption(t *testing.T) {
	opts := []acpOption{
		{Value: "opus[1m]", Name: "Opus"},
		{Value: "sonnet", Name: "Sonnet"},
		{Value: "", Name: "Blank"},
	}
	got := parseClaudeModelOption(opts)
	if len(got) != 2 || got[0].ID != "opus[1m]" || got[0].Name != "Opus" || got[1].ID != "sonnet" {
		t.Fatalf("parseClaudeModelOption = %v", got)
	}
}

// TestACPModelHandshake proves the production probe sends initialize + session/new, refuses an
// adapter-to-client request instead of deadlocking, and preserves each provider's raw result for
// the existing parser.
func TestACPModelHandshake(t *testing.T) {
	cases := []struct {
		agent  string
		result string
		want   string
	}{
		{
			agent: "claude",
			result: `{"sessionId":"c1","configOptions":[{"id":"model","options":[` +
				`{"value":"opus[1m]","name":"Opus"}]}]}`,
			want: "opus[1m]",
		},
		{
			agent:  "gemini",
			result: `{"sessionId":"g1","models":{"availableModels":[{"modelId":"gemini-live","name":"Live"}]}}`,
			want:   "gemini-live",
		},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			child, adapterDone := fakeACPModelChild(json.RawMessage(tc.result))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			result, err := acpModelHandshake(ctx, child, "/repo")
			cancel()
			if err != nil {
				child.Stop()
				t.Fatal(err)
			}
			if err := <-adapterDone; err != nil {
				child.Stop()
				t.Fatal(err)
			}
			child.Stop()
			models := parseACPModelResult(tc.agent, result)
			if len(models) != 1 || models[0].ID != tc.want {
				t.Fatalf("%s models = %v, want %q", tc.agent, models, tc.want)
			}
		})
	}
}

func TestACPModelHandshakeTimeout(t *testing.T) {
	toAdapterR, toAdapterW := io.Pipe()
	fromAdapterR, fromAdapterW := io.Pipe()
	child := &acpproxy.Child{
		In:  toAdapterW,
		Out: fromAdapterR,
		Stop: func() {
			_ = toAdapterR.Close()
			_ = toAdapterW.Close()
			_ = fromAdapterR.Close()
			_ = fromAdapterW.Close()
		},
	}
	go func() {
		var req map[string]any
		_ = json.NewDecoder(toAdapterR).Decode(&req) // consume initialize, then deliberately hang
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err := acpModelHandshake(ctx, child, "/repo")
	cancel()
	child.Stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung adapter error = %v, want context deadline", err)
	}
}

func fakeACPModelChild(sessionResult json.RawMessage) (*acpproxy.Child, <-chan error) {
	toAdapterR, toAdapterW := io.Pipe()
	fromAdapterR, fromAdapterW := io.Pipe()
	var stopOnce sync.Once
	child := &acpproxy.Child{
		In:  toAdapterW,
		Out: fromAdapterR,
		Stop: func() {
			stopOnce.Do(func() {
				_ = toAdapterR.Close()
				_ = toAdapterW.Close()
				_ = fromAdapterR.Close()
				_ = fromAdapterW.Close()
			})
		},
	}
	done := make(chan error, 1)
	go func() {
		dec := json.NewDecoder(toAdapterR)
		enc := json.NewEncoder(fromAdapterW)
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := dec.Decode(&req); err != nil || req.ID != 1 || req.Method != "initialize" {
			done <- fmt.Errorf("initialize request = %+v, err=%v", req, err)
			return
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{}}); err != nil {
			done <- err
			return
		}
		if err := dec.Decode(&req); err != nil || req.ID != 2 || req.Method != "session/new" || req.Params["cwd"] != "/repo" {
			done <- fmt.Errorf("session/new request = %+v, err=%v", req, err)
			return
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": "fs-1", "method": "fs/read_text_file", "params": map[string]any{}}); err != nil {
			done <- err
			return
		}
		var refusal struct {
			ID    string `json:"id"`
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := dec.Decode(&refusal); err != nil || refusal.ID != "fs-1" || refusal.Error.Code != -32601 {
			done <- fmt.Errorf("client-request refusal = %+v, err=%v", refusal, err)
			return
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "result": sessionResult}); err != nil {
			done <- err
			return
		}
		done <- nil
	}()
	return child, done
}

// TestRefreshModelsUsesACPFetcher locks the command boundary: --refresh invokes the boxed fetcher
// for Claude/Gemini and writes its result; failure preserves a prior cache; no --refresh never
// touches the fetcher (and therefore stays runtime-free).
func TestRefreshModelsUsesACPFetcher(t *testing.T) {
	for _, agent := range []string{"claude", "gemini"} {
		t.Run(agent+" success", func(t *testing.T) {
			a := modelsApp(t)
			calls := 0
			a.acpModels = func(got string) ([]modelInfo, error) {
				calls++
				if got != agent {
					t.Fatalf("fetch agent = %q, want %q", got, agent)
				}
				return []modelInfo{{ID: agent + "-live", Name: "Live"}}, nil
			}
			if code, err := a.cmdModels([]string{agent, "--refresh"}); code != 0 || err != nil {
				t.Fatalf("cmdModels = (%d, %v)", code, err)
			}
			if calls != 1 {
				t.Fatalf("ACP fetch calls = %d, want 1", calls)
			}
			mc, ok := loadModelsCache(a.cfg, agent)
			if !ok || len(mc.Models) != 1 || mc.Models[0].ID != agent+"-live" {
				t.Fatalf("cache = (%+v, %v)", mc, ok)
			}
		})
	}

	t.Run("failure preserves cache", func(t *testing.T) {
		a := modelsApp(t)
		if err := writeModelsCache(a.cfg, "claude", []modelInfo{{ID: "still-good"}}); err != nil {
			t.Fatal(err)
		}
		a.acpModels = func(string) ([]modelInfo, error) { return nil, errors.New("box down") }
		out := captureStdout(t, func() { _, _ = a.cmdModels([]string{"claude", "--refresh"}) })
		if !strings.Contains(out, "still-good") || !strings.Contains(out, "refresh failed") {
			t.Fatalf("failed refresh did not preserve/describe the cache:\n%s", out)
		}
	})

	t.Run("plain stays local", func(t *testing.T) {
		a := modelsApp(t)
		a.acpModels = func(string) ([]modelInfo, error) { panic("plain models invoked ACP") }
		if code, err := a.cmdModels([]string{"claude"}); code != 0 || err != nil {
			t.Fatalf("plain cmdModels = (%d, %v)", code, err)
		}
	})
}

// TestModelsDisplayPrefersLiveCache: with a warm cache, an agent's block shows the cached
// ids and a fresh "Last refreshed"; with no cache it shows the static Models() and says the
// list was never refreshed — freshness is an explicit fact, not a tag.
func TestModelsDisplayPrefersLiveCache(t *testing.T) {
	a := modelsApp(t)
	// Cold: the claude block shows the static list and "never".
	cold := captureStdout(t, func() { a.cmdModels([]string{"claude"}) })
	if !strings.Contains(cold, "claude-sonnet-5") || !strings.Contains(cold, "Last refreshed: never") {
		t.Errorf("cold block should show the static list and a never-refreshed line:\n%s", cold)
	}
	// Warm: the claude block shows the cached id and when it was fetched.
	if err := writeModelsCache(a.cfg, "claude", []modelInfo{{ID: "opus-live-xyz"}}); err != nil {
		t.Fatal(err)
	}
	warm := captureStdout(t, func() { a.cmdModels([]string{"claude"}) })
	if !strings.Contains(warm, "opus-live-xyz") || !strings.Contains(warm, "Last refreshed: just now") {
		t.Errorf("warm block should show the cached id and a just-now refresh line:\n%s", warm)
	}
}

// TestRefreshFallsBackToStatic: --refresh for an agent whose native CLI is absent (codex/grok
// not on PATH under a scrubbed PATH) writes no cache, so the block stays static and its
// Last-refreshed line says the refresh failed — never an error, never a blank menu.
func TestRefreshFallsBackToStatic(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no codex/grok binary reachable → every native fetch fails
	a := modelsApp(t)
	out := captureStdout(t, func() {
		if code, err := a.cmdModels([]string{"codex", "--refresh"}); code != 0 || err != nil {
			t.Fatalf("cmdModels --refresh = (%d, %v), want a clean (0, nil)", code, err)
		}
	})
	if _, ok := loadModelsCache(a.cfg, "codex"); ok {
		t.Error("a failed refresh must not write a cache")
	}
	if !strings.Contains(out, "gpt-5.6-sol") || !strings.Contains(out, "gpt-5.3-codex-spark") ||
		!strings.Contains(out, "refresh failed") {
		t.Errorf("after a failed refresh the codex block should stay static and note the failure:\n%s", out)
	}
	for _, removed := range []string{"gpt-5-codex", "gpt-5 ·", "o4-mini"} {
		if strings.Contains(out, removed) {
			t.Errorf("failed-refresh fallback still advertises removed model %q:\n%s", removed, out)
		}
	}
}
