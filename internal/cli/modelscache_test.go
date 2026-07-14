package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
