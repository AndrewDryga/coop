package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

// modelsCacheTTL is how long a fetched model list counts as "live" for `coop models`. Past
// it, coop falls back to the curated static Models() — honest examples beat a stale
// "(live)". The cache refreshes opportunistically on `coop acp` and on demand with
// `coop models --refresh` through each provider's real catalog source.
const modelsCacheTTL = 14 * 24 * time.Hour

// modelFetchTimeout bounds both native-CLI probes and the Claude/Gemini ACP handshake so
// `coop models --refresh` can never hang.
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

// loadModelsCache reads agent's cached model list. The bool reports whether the list is
// USABLE as live — present, parseable, non-empty, and within the TTL — so a caller trusts
// mc.Models only on true. An expired cache returns (mc, false) with FetchedAt intact, so
// the menu can say HOW stale it is; anything unreadable returns a zero modelsCache. The
// caller falls back to the static Models(). Never blocks or spawns anything.
func loadModelsCache(cfg *config.Config, agent string) (modelsCache, bool) {
	b, err := os.ReadFile(modelsCachePath(cfg, agent))
	if err != nil {
		return modelsCache{}, false
	}
	var mc modelsCache
	if json.Unmarshal(b, &mc) != nil || len(mc.Models) == 0 {
		return modelsCache{}, false
	}
	if time.Since(mc.FetchedAt) > modelsCacheTTL {
		return mc, false
	}
	return mc, true
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

// nativeModelFetchers maps an agent to its auth-free host-CLI model probe (grok/codex).
// Claude/Gemini advertise models only through ACP and use fetchACPModelCatalog below.
var nativeModelFetchers = map[string]func() ([]modelInfo, error){
	"grok":  fetchGrokModels,
	"codex": fetchCodexModels,
}

// runModelCLI runs an agent's auth-free list command on the host and returns its stdout,
// timeout-bounded. An error (CLI not on PATH, non-zero exit, timeout) tells the caller to
// keep the cached/static list. The catalog these commands print is credential-independent,
// so the host's own login (or none) suffices and these two provider probes need no box.
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

// fetchModelCatalog selects the cheapest real catalog source per provider. The ACP seam is
// deliberately app-local: unit tests can prove refresh wiring without detecting a runtime, while
// production always launches the real credential-scoped box.
func (a *app) fetchModelCatalog(agent string) ([]modelInfo, error) {
	if fetch := nativeModelFetchers[agent]; fetch != nil {
		return fetch()
	}
	if agent != "claude" && agent != "gemini" {
		return nil, fmt.Errorf("%s has no model fetcher", agent)
	}
	if a.acpModels != nil {
		return a.acpModels(agent)
	}
	return a.fetchACPModelCatalog(agent)
}

// fetchACPModelCatalog launches one inner ACP box, asks for a fresh session's advertised models,
// then tears down both its process group and any container generation carrying this exact
// supervisor id. It bypasses the public supervisor: a two-request probe needs no warm pool,
// restart replay, or editor control layer.
func (a *app) fetchACPModelCatalog(agent string) ([]modelInfo, error) {
	if err := a.ensureRuntime(); err != nil {
		return nil, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	superID, err := newSupervisorID()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelFetchTimeout)
	defer cancel()
	child, err := a.spawnBox(ctx, self, []string{"acp", agent}, superID, nil, agents.Target{Provider: agent}, "", true, io.Discard)
	if err != nil {
		return nil, err
	}
	result, fetchErr := acpModelHandshake(ctx, child, repo)
	child.Stop()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), acpCleanupTimeout)
	_, cleanupErr := a.rt.RemoveByLabel(cleanupCtx, box.LabelSupervisor, superID)
	cleanupCancel()
	if fetchErr != nil {
		if cleanupErr != nil {
			return nil, errors.Join(fetchErr, cleanupErr)
		}
		return nil, fetchErr
	}
	if cleanupErr != nil {
		return nil, cleanupErr
	}
	models := parseACPModelResult(agent, result)
	if len(models) == 0 {
		return nil, errors.New("ACP session advertised no models")
	}
	return models, nil
}

// acpModelHandshake drives only the setup needed to make adapters advertise their model catalog.
// It still handles adapter-to-client requests so a provider cannot deadlock the probe waiting on a
// capability the non-editor client does not implement.
func acpModelHandshake(ctx context.Context, child *acpproxy.Child, cwd string) (json.RawMessage, error) {
	r := bufio.NewReaderSize(child.Out, 1<<20)
	initialize := map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	}
	if _, err := acpRoundTrip(ctx, child.In, r, 1, "initialize", initialize); err != nil {
		return nil, err
	}
	return acpRoundTrip(ctx, child.In, r, 2, "session/new", map[string]any{
		"cwd": cwd, "mcpServers": []any{},
	})
}

func acpRoundTrip(ctx context.Context, w io.Writer, r *bufio.Reader, id int, method string, params any) (json.RawMessage, error) {
	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	if err := writeACPMessage(ctx, w, request); err != nil {
		return nil, err
	}
	for {
		line, err := readACPLine(ctx, r)
		if err != nil {
			return nil, err
		}
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			return nil, fmt.Errorf("decode ACP response: %w", err)
		}
		if frame.Method != "" && len(frame.ID) > 0 {
			if err := writeACPMessage(ctx, w, map[string]any{
				"jsonrpc": "2.0", "id": frame.ID,
				"error": map[string]any{"code": -32601, "message": "no capability"},
			}); err != nil {
				return nil, err
			}
			continue
		}
		if string(bytes.TrimSpace(frame.ID)) != strconv.Itoa(id) {
			continue // notification or a response to the refused adapter request
		}
		if len(frame.Error) > 0 && string(bytes.TrimSpace(frame.Error)) != "null" {
			return nil, fmt.Errorf("ACP %s failed: %s", method, frame.Error)
		}
		if len(frame.Result) == 0 {
			return nil, fmt.Errorf("ACP %s returned no result", method)
		}
		return frame.Result, nil
	}
}

func writeACPMessage(ctx context.Context, w io.Writer, msg any) error {
	done := make(chan error, 1)
	go func() { done <- json.NewEncoder(w).Encode(msg) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func readACPLine(ctx context.Context, r *bufio.Reader) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := r.ReadBytes('\n')
		done <- result{line: line, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			return nil, got.err
		}
		return got.line, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func parseACPModelResult(agent string, result json.RawMessage) []modelInfo {
	var doc struct {
		Models        json.RawMessage `json:"models"`
		ConfigOptions []struct {
			ID      string      `json:"id"`
			Options []acpOption `json:"options"`
		} `json:"configOptions"`
	}
	if json.Unmarshal(result, &doc) != nil {
		return nil
	}
	if agent == "gemini" {
		return parseGeminiModels(doc.Models)
	}
	if agent == "claude" {
		for _, option := range doc.ConfigOptions {
			if option.ID == "model" {
				return parseClaudeModelOption(option.Options)
			}
		}
	}
	return nil
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
