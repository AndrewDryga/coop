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
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// TestModelsCacheRoundTrip: a written cache reads back current, carrying both stamps; the
// synthetic "default" choice never reaches the ids; an empty write is a no-op that never clobbers
// a good cache.
func TestModelsCacheRoundTrip(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	if _, ok := loadModelsCache(cfg, "claude"); ok {
		t.Fatal("a cold cache must not read as present")
	}
	want := []agents.Model{{ID: "default", Name: "Default (recommended)"}, {ID: "opus"}, {ID: "sonnet"}}
	if err := writeModelsCache(cfg, "claude", want); err != nil {
		t.Fatal(err)
	}
	got, ok := loadModelsCache(cfg, "claude")
	if !ok || len(got.Models) != 3 {
		t.Fatalf("warm cache = (%v, %v), want the three written models", got, ok)
	}
	if ids := got.ids(); len(ids) != 2 || ids[0] != "opus" || ids[1] != "sonnet" {
		t.Errorf("ids() = %v, want the synthetic default dropped", ids)
	}
	now := time.Now()
	if !got.current(now) || !got.usable(now) || got.due(now) {
		t.Errorf("a just-written cache should be current, usable and not due: %+v", got)
	}
	if got.AttemptedAt.IsZero() || got.AttemptError != "" {
		t.Errorf("a success should stamp the attempt and clear any failure: %+v", got)
	}
	// An empty fetch is a no-op — it must not wipe the good cache.
	if err := writeModelsCache(cfg, "claude", nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := loadModelsCache(cfg, "claude"); !ok || len(got.Models) != 3 {
		t.Fatalf("empty write clobbered the cache: (%v, %v)", got, ok)
	}
}

func TestModelCatalogHostDispatchAndFailure(t *testing.T) {
	for _, tc := range []struct{ provider, args, output, id, cause string }{
		{"codex", "debug models", `{"models":[{"slug":"native-codex","visibility":"list"}]}`, "native-codex", ""},
		{"grok", "models", "  * native-grok (default)", "native-grok", ""},
		{"grok", "models", "You are not authenticated.\n  * grok-build (default)", "", "The host grok CLI is not signed in."},
	} {
		t.Run(tc.provider+tc.id, func(t *testing.T) {
			a := modelsApp(t)
			dir := t.TempDir()
			// An independent executable pins host argv; the injected ACP seam must not run.
			argv := strings.Fields(tc.args)
			script := fmt.Sprintf("#!/bin/sh\n[ \"$#\" -eq %d ] || exit 92\n", len(argv))
			for i, arg := range argv {
				script += fmt.Sprintf("[ \"${%d}\" = '%s' ] || exit 92\n", i+1, arg)
			}
			script += "printf '%s\\n' '" + tc.output + "'\n"
			if err := os.WriteFile(filepath.Join(dir, tc.provider), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			a.acpModels = func(string) ([]agents.Model, error) { t.Fatal("host discovery reached ACP seam"); return nil, nil }
			if err := writeModelsCache(a.cfg, tc.provider, []agents.Model{{ID: "last-good"}}); err != nil {
				t.Fatal(err)
			}
			cause, failed := a.refreshCatalog(tc.provider, nil)
			if cause != tc.cause || failed != (tc.cause != "") {
				t.Fatalf("refresh = %q/%v", cause, failed)
			}
			cache, ok := loadModelsCache(a.cfg, tc.provider)
			want := tc.id
			if failed {
				want = "last-good"
			}
			if !ok || len(cache.Models) != 1 || cache.Models[0].ID != want || cache.AttemptError != tc.cause {
				t.Fatalf("cache = %+v/%v, want %s", cache, ok, want)
			}
		})
	}
}

func TestModelCatalogNeedsBox(t *testing.T) {
	a := modelsApp(t)
	for _, seam := range []bool{false, true} {
		a.acpModels = nil
		if seam {
			a.acpModels = func(string) ([]agents.Model, error) { return nil, nil }
		}
		for _, name := range agents.Names() {
			want := !seam && (name == "claude" || name == "gemini")
			if got := a.fetchNeedsBox(name); got != want {
				t.Errorf("%s seam=%v needs box=%v, want %v", name, seam, got, want)
			}
		}
	}
}

// TestModelsCacheAges: the three ages answer different questions — refresh-due at 24h, still
// presentable as last-known until the retention horizon, and never both.
func TestModelsCacheAges(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                          string
		age                           time.Duration
		current, usable, due, showsID bool
	}{
		{"fresh", time.Hour, true, true, false, true},
		{"past the refresh age", 30 * time.Hour, false, true, true, true},
		{"past retention", modelsCacheRetention + time.Hour, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := modelsApp(t)
			seedModelsCache(t, a.cfg, "claude", now.Add(-tc.age), time.Time{}, "", "cached-id")
			mc, _ := loadModelsCache(a.cfg, "claude")
			if mc.current(now) != tc.current || mc.usable(now) != tc.usable || mc.due(now) != tc.due {
				t.Errorf("current/usable/due = %v/%v/%v, want %v/%v/%v",
					mc.current(now), mc.usable(now), mc.due(now), tc.current, tc.usable, tc.due)
			}
			out := captureStdout(t, func() { _, _ = a.cmdModels([]string{"claude"}) })
			if strings.Contains(out, "cached-id") != tc.showsID {
				t.Errorf("cached id shown = %v, want %v:\n%s", !tc.showsID, tc.showsID, out)
			}
		})
	}
}

// seedModelsCache writes agent's cache file with exactly the stamps a test needs — the only way to
// reach an age or a recorded failure without waiting for one.
func seedModelsCache(t *testing.T, cfg *config.Config, agent string, fetched, attempted time.Time, attemptErr string, ids ...string) {
	t.Helper()
	mc := modelsCache{FetchedAt: fetched, AttemptedAt: attempted, AttemptError: attemptErr}
	for _, id := range ids {
		mc.Models = append(mc.Models, agents.Model{ID: id})
	}
	b, err := json.Marshal(mc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.ConfigDir, agent), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelsCachePath(cfg, agent), b, 0o600); err != nil {
		t.Fatal(err)
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
			a.acpModels = func(got string) ([]agents.Model, error) {
				calls++
				if got != agent {
					t.Fatalf("fetch agent = %q, want %q", got, agent)
				}
				return []agents.Model{{ID: agent + "-live", Name: "Live"}}, nil
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
		if err := writeModelsCache(a.cfg, "claude", []agents.Model{{ID: "still-good"}}); err != nil {
			t.Fatal(err)
		}
		a.acpModels = func(string) ([]agents.Model, error) { return nil, errors.New("box down") }
		out := captureStdout(t, func() { _, _ = a.cmdModels([]string{"claude", "--refresh"}) })
		if !strings.Contains(out, "still-good") || !strings.Contains(out, "Could not refresh") {
			t.Fatalf("failed refresh did not preserve/describe the cache:\n%s", out)
		}
	})

}

// TestModelsRefreshesOnlyWhatIsDue is the upkeep contract of the plain menu: a catalog inside the
// refresh age costs no fetch, an older one is refetched silently, and a failure inside the retry
// window is not paid for twice — while --refresh ignores both.
func TestModelsRefreshesOnlyWhatIsDue(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name              string
		fetched, attempts time.Time
		args              []string
		wantCalls         int
	}{
		{"fresh costs no fetch", now.Add(-time.Hour), time.Time{}, []string{"claude"}, 0},
		{"stale is refetched", now.Add(-30 * time.Hour), time.Time{}, []string{"claude"}, 1},
		{"a recent failure backs off", now.Add(-30 * time.Hour), now.Add(-time.Minute), []string{"claude"}, 0},
		{"--refresh ignores freshness", now.Add(-time.Hour), time.Time{}, []string{"claude", "--refresh"}, 1},
		{"--refresh ignores the backoff", now.Add(-30 * time.Hour), now.Add(-time.Minute), []string{"claude", "--refresh"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := modelsApp(t)
			seedModelsCache(t, a.cfg, "claude", tc.fetched, tc.attempts, "provider was down", "cached-id")
			calls := 0
			a.acpModels = func(string) ([]agents.Model, error) {
				calls++
				return []agents.Model{{ID: "refetched-id"}}, nil
			}
			out := captureStdout(t, func() {
				if code, err := a.cmdModels(tc.args); code != 0 || err != nil {
					t.Fatalf("cmdModels = (%d, %v)", code, err)
				}
			})
			if calls != tc.wantCalls {
				t.Errorf("provider fetches = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantCalls > 0 {
				// A successful refresh shows the new ids and says nothing about having done it.
				if !strings.Contains(out, "refetched-id") || strings.Contains(out, "⚠") {
					t.Errorf("a successful refresh should be silent and current:\n%s", out)
				}
			}
		})
	}
}

// TestModelsBacksOffWithTheRecordedCause: inside the retry window the menu still explains itself —
// last-known ids, one warning on that agent, naming the cause the failed attempt recorded.
func TestModelsBacksOffWithTheRecordedCause(t *testing.T) {
	a := modelsApp(t)
	seedModelsCache(t, a.cfg, "claude", time.Now().Add(-50*time.Hour), time.Now().Add(-time.Minute),
		"Docker is not running", "cached-id")
	a.acpModels = func(string) ([]agents.Model, error) { t.Fatal("backed-off agent was refetched"); return nil, nil }
	out := captureStdout(t, func() { _, _ = a.cmdModels([]string{"claude"}) })
	want := "  ⚠ Could not refresh — showing the list saved 2 days ago\n\n      Docker is not running\n"
	if !strings.Contains(out, want) || !strings.Contains(out, "cached-id") {
		t.Errorf("menu missing %q with its last-known ids:\n%s", want, out)
	}
}

// TestModelsFailureWarnsOnlyTheAffectedAgent: one provider's refresh failing keeps the command
// useful — its own last-known list under one honest warning — and leaves every other agent alone.
func TestModelsFailureWarnsOnlyTheAffectedAgent(t *testing.T) {
	a := modelsApp(t)
	seedModelsCache(t, a.cfg, "claude", time.Now().Add(-30*time.Hour), time.Time{}, "", "still-good")
	seedModelsCache(t, a.cfg, "gemini", time.Now().Add(-time.Hour), time.Time{}, "", "gemini-fresh-id")
	a.acpModels = func(agent string) ([]agents.Model, error) {
		return nil, errors.New("box down")
	}
	out := captureStdout(t, func() { _, _ = a.cmdModels(nil) })
	if !strings.Contains(out, "still-good") {
		t.Errorf("a failed refresh must keep the last-known list:\n%s", out)
	}
	if n := strings.Count(out, "⚠"); n != 3 { // claude (last-known) + codex and grok (no CLI on PATH)
		t.Errorf("warnings = %d, want one per affected agent:\n%s", n, out)
	}
	gemini := out[strings.Index(out, "Gemini"):]
	if strings.Contains(gemini[:strings.Index(gemini, "Grok")], "⚠") {
		t.Errorf("a fresh agent must not be turned into a warning:\n%s", out)
	}
	// The failure is recorded so the next read backs off instead of paying the same timeout.
	mc, _ := loadModelsCache(a.cfg, "claude")
	if mc.AttemptedAt.IsZero() || mc.due(time.Now()) {
		t.Errorf("a failed attempt should be stamped and back off: %+v", mc)
	}
	if len(mc.ids()) != 1 || mc.ids()[0] != "still-good" {
		t.Errorf("a failed attempt must not disturb the models it holds: %+v", mc)
	}
}

// TestRefreshFallsBackToExamples: a forced refresh for an agent whose native CLI is absent writes
// no catalog, so the block shows the bundled examples under one warning that names the real cause
// — never an error, never a blank menu.
func TestRefreshFallsBackToExamples(t *testing.T) {
	a := modelsApp(t) // modelsApp already scrubs PATH: no codex binary is reachable
	out := captureStdout(t, func() {
		if code, err := a.cmdModels([]string{"codex", "--refresh"}); code != 0 || err != nil {
			t.Fatalf("cmdModels --refresh = (%d, %v), want a clean (0, nil)", code, err)
		}
	})
	if mc, _ := loadModelsCache(a.cfg, "codex"); len(mc.Models) != 0 {
		t.Error("a failed refresh must not write a catalog")
	}
	want := "  ⚠ Could not refresh — showing example models\n\n      Codex is unavailable on this host.\n"
	if !strings.Contains(out, "gpt-5.6-sol") || !strings.Contains(out, "gpt-5.3-codex-spark") ||
		!strings.Contains(out, want) {
		t.Errorf("after a failed refresh the codex block should show examples and %q:\n%s", want, out)
	}
	for _, removed := range []string{"gpt-5-codex", "gpt-5 ·", "o4-mini"} {
		if strings.Contains(out, removed) {
			t.Errorf("failed-refresh fallback still advertises removed model %q:\n%s", removed, out)
		}
	}
}
