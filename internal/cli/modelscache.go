package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// modelsCacheTTL is how long a fetched model list counts as "live" for `coop models`. Past
// it, coop falls back to the curated static Models() — an honest "(examples)" beats a stale
// "(live)". The cache refreshes for free on every `coop acp` session (claude/gemini) and on
// demand with `coop models --refresh` (grok/codex, via their auth-free native CLI).
const modelsCacheTTL = 14 * 24 * time.Hour

// modelFetchTimeout bounds a native-CLI model probe so `coop models --refresh` can never hang.
const modelFetchTimeout = 15 * time.Second

// modelInfo is one model in the cache: the id the agent's CLI accepts (what --model takes),
// plus an optional friendly display name.
type modelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// modelsCache is the per-agent live model list coop keeps under the agent's config dir.
type modelsCache struct {
	Models    []modelInfo `json:"models"`
	FetchedAt time.Time   `json:"fetchedAt"`
}

// modelsCachePath is <ConfigDir>/<agent>/models_cache.json — sibling to the agent's
// profiles/, since the model catalog is a per-agent fact, not a per-credential one.
func modelsCachePath(cfg *config.Config, agent string) string {
	return filepath.Join(cfg.ConfigDir, agent, "models_cache.json")
}

// loadModelsCache reads agent's cached live models. The bool reports whether the list is
// USABLE as "live" — present, parseable, non-empty, and within the TTL — so a caller shows
// "(live)" only on true. Any problem (absent, corrupt, empty, expired) returns (nil, false)
// with no error: the caller falls back to the static Models(). Never blocks or spawns anything.
func loadModelsCache(cfg *config.Config, agent string) ([]modelInfo, bool) {
	b, err := os.ReadFile(modelsCachePath(cfg, agent))
	if err != nil {
		return nil, false
	}
	var mc modelsCache
	if json.Unmarshal(b, &mc) != nil || len(mc.Models) == 0 {
		return nil, false
	}
	if time.Since(mc.FetchedAt) > modelsCacheTTL {
		return nil, false
	}
	return mc.Models, true
}

// writeModelsCache atomically replaces agent's cache with models, stamped now. Best-effort:
// --refresh surfaces a returned error; the free opportunistic ACP path ignores it. An empty
// list is a no-op — a failed fetch must never clobber a good cache. A unique temp file plus
// rename keeps a concurrent writer (the ACP box→editor goroutine) from corrupting the file.
func writeModelsCache(cfg *config.Config, agent string, models []modelInfo) error {
	if len(models) == 0 {
		return nil
	}
	dir := filepath.Join(cfg.ConfigDir, agent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(modelsCache{Models: models, FetchedAt: time.Now()})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "models_cache-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, modelsCachePath(cfg, agent))
}

// nativeModelFetchers maps an agent to its auth-free host-CLI model probe (grok/codex). The
// other agents (claude/gemini) have no cheap native list — their cache is populated for free
// during `coop acp`, where the proxy already parses the ACP session/new models.
var nativeModelFetchers = map[string]func() ([]modelInfo, error){
	"grok":  fetchGrokModels,
	"codex": fetchCodexModels,
}

// runModelCLI runs an agent's auth-free list command on the host and returns its stdout,
// timeout-bounded. An error (CLI not on PATH, non-zero exit, timeout) tells the caller to
// keep the cached/static list. The catalog these commands print is credential-independent,
// so the host's own login (or none) suffices — no box, so --refresh stays Docker-free.
func runModelCLI(name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelFetchTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// fetchGrokModels lists grok's models via `grok models`.
func fetchGrokModels() ([]modelInfo, error) {
	out, err := runModelCLI("grok", "models")
	if err != nil {
		return nil, err
	}
	return parseGrokModels(out), nil
}

// fetchCodexModels lists codex's models via `codex debug models`.
func fetchCodexModels() ([]modelInfo, error) {
	out, err := runModelCLI("codex", "debug", "models")
	if err != nil {
		return nil, err
	}
	return parseCodexModels(out)
}

// parseGrokModels reads `grok models` output — a bullet per model, the default marked:
//
//   - grok-4.5 (default)
//   - grok-composer-2.5-fast
//
// It returns ids in listed order (name = id; grok prints no separate display name), skipping
// blanks and duplicates.
func parseGrokModels(out []byte) []modelInfo {
	var models []modelInfo
	seen := map[string]bool{}
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		rest, ok := strings.CutPrefix(line, "* ")
		if !ok {
			rest, ok = strings.CutPrefix(line, "- ")
		}
		if !ok {
			continue
		}
		fields := strings.Fields(rest) // the id, then an optional " (default)" marker
		if len(fields) == 0 || seen[fields[0]] {
			continue
		}
		seen[fields[0]] = true
		models = append(models, modelInfo{ID: fields[0], Name: fields[0]})
	}
	return models
}

// parseCodexModels reads `codex debug models` JSON, keeping only list-visible models
// (visibility=="list", dropping bundled/internal ones) with their slug + display name.
func parseCodexModels(out []byte) ([]modelInfo, error) {
	var doc struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	var models []modelInfo
	for _, m := range doc.Models {
		if m.Visibility != "list" || m.Slug == "" {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = m.Slug
		}
		models = append(models, modelInfo{ID: m.Slug, Name: name})
	}
	return models, nil
}

// parseGeminiModels reads a gemini ACP session/new `models` field
// ({availableModels:[{modelId,name}], currentModelId}) into cache entries — the same shape
// synthModelOption renders as the toolbar dropdown. Empty/absent → nil.
func parseGeminiModels(models json.RawMessage) []modelInfo {
	if len(models) == 0 {
		return nil
	}
	var m struct {
		AvailableModels []struct {
			ModelID string `json:"modelId"`
			Name    string `json:"name"`
		} `json:"availableModels"`
	}
	if json.Unmarshal(models, &m) != nil {
		return nil
	}
	out := make([]modelInfo, 0, len(m.AvailableModels))
	for _, am := range m.AvailableModels {
		if am.ModelID == "" {
			continue
		}
		out = append(out, modelInfo{ID: am.ModelID, Name: am.Name})
	}
	return out
}

// parseClaudeModelOption reads a claude configOptions `model` select (options:[{value,name}])
// into cache entries — the ids coop already offers in the ACP toolbar.
func parseClaudeModelOption(options []acpOption) []modelInfo {
	out := make([]modelInfo, 0, len(options))
	for _, o := range options {
		if o.Value == "" {
			continue
		}
		out = append(out, modelInfo{ID: o.Value, Name: o.Name})
	}
	return out
}
