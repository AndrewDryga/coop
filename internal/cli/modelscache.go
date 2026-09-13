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
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
)

// The three ages that drive `coop models`, kept apart on purpose:
//   - modelsRefreshAfter — past this a catalog is refetched on the next menu read, so upkeep is
//     coop's job and not a chore the help has to teach.
//   - modelsCacheRetention — past this a list is too old to show even as last-known, and the
//     release-bundled examples are the honest thing to print.
//   - modelsRetryAfter — how long a FAILED attempt suppresses the next one, so an uninstalled CLI
//     or a stopped runtime is paid for once an hour, not on every invocation.
const (
	modelsRefreshAfter   = 24 * time.Hour
	modelsCacheRetention = 14 * 24 * time.Hour
	modelsRetryAfter     = time.Hour
)

// modelFetchTimeout bounds both native-CLI probes and the Claude/Gemini ACP handshake so a
// catalog refresh can never hang the menu.
const modelFetchTimeout = 15 * time.Second

// modelsCache retains the last good catalog separately from the most recent fetch
// attempt, so a failed refresh can back off without discarding usable model ids.
type modelsCache struct {
	Models       []agents.Model `json:"models"`
	FetchedAt    time.Time      `json:"fetchedAt"`
	AttemptedAt  time.Time      `json:"attemptedAt,omitempty"`
	AttemptError string         `json:"attemptError,omitempty"`
}

// ids are the model ids to offer a human: the catalog's own order, minus the synthetic "default"
// choice — leaving a model out of a target already selects the provider's default, so listing it
// as an id only invites someone to type a word that isn't a model.
func (mc modelsCache) ids() []string {
	out := make([]string, 0, len(mc.Models))
	for _, m := range mc.Models {
		if m.ID == "" || m.ID == "default" {
			continue
		}
		out = append(out, m.ID)
	}
	return out
}

// current reports whether the cached list still counts as this agent's catalog — a successful
// fetch inside modelsRefreshAfter. Anything older is shown as last-known, never as current.
func (mc modelsCache) current(now time.Time) bool {
	return !mc.FetchedAt.IsZero() && now.Sub(mc.FetchedAt) < modelsRefreshAfter
}

// usable reports whether the cached list is still safe to present as last-known.
func (mc modelsCache) usable(now time.Time) bool {
	return !mc.FetchedAt.IsZero() && now.Sub(mc.FetchedAt) < modelsCacheRetention
}

// due reports whether this agent's catalog should be refetched now: not current, and not inside
// the retry window a failed attempt left behind.
func (mc modelsCache) due(now time.Time) bool {
	return !mc.current(now) && now.Sub(mc.AttemptedAt) >= modelsRetryAfter
}

// modelsCachePath is <ConfigDir>/<agent>/models_cache.json — sibling to the agent's
// profiles/, since the model catalog is a per-agent fact, not a per-credential one.
func modelsCachePath(cfg *config.Config, agent string) string {
	return filepath.Join(cfg.ConfigDir, agent, "models_cache.json")
}

// loadModelsCache reads agent's cache. The bool reports only whether the file was there and
// parseable — freshness, usability and whether a refetch is due are the modelsCache methods above,
// because a caller that wants the attempt stamps needs them even when the list itself is empty.
// Anything unreadable returns a zero modelsCache. Never blocks or spawns anything.
func loadModelsCache(cfg *config.Config, agent string) (modelsCache, bool) {
	b, err := os.ReadFile(modelsCachePath(cfg, agent))
	if err != nil {
		return modelsCache{}, false
	}
	var mc modelsCache
	if json.Unmarshal(b, &mc) != nil {
		return modelsCache{}, false
	}
	return mc, true
}

// writeModelsCache atomically records a SUCCESSFUL fetch: models, stamped now as both the last
// success and the last attempt, clearing any recorded failure. An empty list is a no-op — a failed
// fetch must never clobber a good cache.
func writeModelsCache(cfg *config.Config, agent string, models []agents.Model) error {
	if len(models) == 0 {
		return nil
	}
	now := time.Now()
	return storeModelsCache(cfg, agent, modelsCache{Models: models, FetchedAt: now, AttemptedAt: now})
}

// recordModelsFetchFailure stamps a FAILED fetch on agent's cache without touching the models it
// already holds: the attempt time is what backs the next read off, and cause is what that read
// shows beside the agent instead of pretending the list is current.
func recordModelsFetchFailure(cfg *config.Config, agent, cause string) error {
	mc, _ := loadModelsCache(cfg, agent)
	mc.AttemptedAt, mc.AttemptError = time.Now(), cause
	return storeModelsCache(cfg, agent, mc)
}

// storeModelsCache atomically replaces agent's cache file. Best-effort: a forced refresh surfaces
// a returned error; the free opportunistic ACP path ignores it. A unique temp file plus rename
// keeps a concurrent writer (the ACP box→editor goroutine) from corrupting the file. No fsync
// (unlike config.WriteFileAtomic): this is a cache of a remote catalog, so a crash losing the last
// write costs one refetch — paying for durability would be theater.
func storeModelsCache(cfg *config.Config, agent string, mc modelsCache) error {
	dir := filepath.Join(cfg.ConfigDir, agent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(mc)
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

// runModelCLI runs an agent's list command on the host and returns its stdout, timeout-bounded. An
// error (CLI not on PATH, non-zero exit, timeout) tells the caller to keep the cached/static list.
// These two probes need no box because they ask the HOST's own CLI — which is also why each
// fetcher has to judge the answer: codex prints its full catalog either way, grok answers a
// logged-out CLI with a placeholder.
func runModelCLI(name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelFetchTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// fetchModelCatalog selects the cheapest real catalog source per provider. The ACP seam is
// deliberately app-local: unit tests can prove refresh wiring without detecting a runtime, while
// production always launches the real credential-scoped box.
func (a *app) fetchModelCatalog(agent string) ([]agents.Model, error) {
	ag, ok := agents.Get(agent)
	if !ok {
		return nil, fmt.Errorf("%s has no model fetcher", agent)
	}
	spec := ag.ModelCatalog()
	if len(spec.HostCommand) > 0 {
		out, err := runModelCLI(spec.HostCommand[0], spec.HostCommand[1:]...)
		if err != nil {
			return nil, err
		}
		return spec.ParseHost(out)
	}
	if spec.ParseACP == nil {
		return nil, fmt.Errorf("%s has no model fetcher", agent)
	}
	if a.acpModels != nil {
		return a.acpModels(agent)
	}
	// An ACP catalog comes from a session the provider has to authorize, so a signed-out account
	// can only fail — say so instead of paying for a container that will.
	if !box.ProfileAuthed(a.cfg, agent, a.cfg.ActiveProfile(agent)) {
		return nil, modelFetchError{cause: signInToRefresh(agent)}
	}
	return a.fetchACPModelCatalog(agent)
}

// signInToRefresh is the one sentence a signed-out catalog gets: what is missing, and the exact
// command that fixes it.
func signInToRefresh(agent string) string {
	return fmt.Sprintf("Sign in to %s to refresh its models: coop login %s", titleName(agent), agent)
}

// fetchACPModelCatalog launches one inner ACP box, asks for a fresh session's advertised models,
// then tears down both its process group and any container generation carrying this exact
// supervisor id. It bypasses the public supervisor: a two-request probe needs no warm pool,
// restart replay, or editor control layer.
func (a *app) fetchACPModelCatalog(agent string) ([]agents.Model, error) {
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
	child, err := a.spawnBox(ctx, self, []string{"acp", agent}, superID, nil, agents.Target{Provider: agent}, "", true, io.Discard, forkspace.ExecutionRoleProbe)
	if err != nil {
		return nil, err
	}
	result, fetchErr := acpModelHandshake(ctx, child, repo)
	child.Stop()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), acpCleanupTimeout)
	_, cleanupErr := a.rt.RemoveByLabel(cleanupCtx, box.LabelSupervisor, superID)
	cleanupCancel()
	if cleanupErr == nil {
		if authorityRepo, _, bindingErr := forkspace.ResolveProjectBinding(repo); bindingErr != nil {
			cleanupErr = bindingErr
		} else {
			cleanupErr = forkspace.RemoveDeadExecutionsBySource(authorityRepo, superID)
		}
	}
	// A failed probe reports the cleanup problem too — it may be the reason (errors.Join drops a
	// nil). A probe that ANSWERED does not: it has a real catalog, and cleanup is coop's own
	// housekeeping — the two ACP probes now run side by side, so one sweeping the execution
	// registry can momentarily see the other's record vanish mid-scan. Blaming a provider for
	// that would be a lie, and a record left behind is swept by any later run.
	if fetchErr != nil {
		return nil, errors.Join(fetchErr, cleanupErr)
	}
	models := parseACPModelResult(agent, result)
	if len(models) == 0 {
		return nil, errors.Join(errors.New("ACP session advertised no models"), cleanupErr)
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

func parseACPModelResult(name string, result json.RawMessage) []agents.Model {
	ag, ok := agents.Get(name)
	if !ok || ag.ModelCatalog().ParseACP == nil {
		return nil
	}
	return ag.ModelCatalog().ParseACP(result)
}
