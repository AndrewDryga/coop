package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

type usageBlockingQuotaTransport struct{}

func (usageBlockingQuotaTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	fmt.Fprintln(os.Stdout, "quota-ready")
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestUsageSignalCancelsLookup(t *testing.T) {
	if os.Getenv("COOP_USAGE_SIGNAL_HELPER") == "1" {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("NO_COLOR", "1")
		cfg := &config.Config{ConfigDir: t.TempDir()}
		usageFixtureFile(t, filepath.Join(cfg.AgentProfileDir("claude", "work"), ".credentials.json"), fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"fixture","refreshToken":"fixture","expiresAt":%d,"scopes":["user:profile"]}}`, time.Now().Add(time.Hour).UnixMilli()))
		http.DefaultTransport = usageBlockingQuotaTransport{}
		a := &app{cfg: cfg}
		if code, err := a.cmdUsage([]string{"claude@work"}); code != 1 || err != nil {
			t.Fatalf("cancelled lookup = (%d,%v)", code, err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUsageSignalCancelsLookup$")
	cmd.Env = append(os.Environ(), "COOP_USAGE_SIGNAL_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || line != "quota-ready\n" {
		t.Fatalf("quota did not enter fixture: %q, %v", line, err)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	remaining, _ := io.ReadAll(reader)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("signal killed command instead of cancelling owned lookup: %v\n%s%s", err, remaining, stderr.String())
	}
}

func usageFixtureFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUsageCommandOutsideRepositoryAndExactSelector(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NO_COLOR", "1")
	cfg := &config.Config{ConfigDir: t.TempDir(), RuntimeName: "must-not-run"}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	data := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"r","message":{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":20}}}`, stamp)
	usageFixtureFile(t, filepath.Join(cfg.AgentProfileDir("claude", "work"), "projects", "bucket", "s.jsonl"), data)
	a := &app{cfg: cfg}
	before := cfg.DefaultProfileOf("claude")
	out := captureStdout(t, func() {
		code, err := a.cmdUsage([]string{"claude@work"})
		if code != 0 || err != nil {
			t.Errorf("usage=(%d,%v)", code, err)
		}
	})
	for _, wanted := range []string{"Claude", "work", "30-day API estimate", "≈$", "sign-in required", "coop login claude@work"} {
		if !strings.Contains(out, wanted) {
			t.Fatalf("missing %q in %s", wanted, out)
		}
	}
	if strings.ContainsRune(out, '\x1b') || a.rtSet || cfg.DefaultProfileOf("claude") != before {
		t.Fatal("inspection changed authority or initialized runtime")
	}
	for _, args := range [][]string{{"claude:model"}, {"claude/high"}, {"claude@a,b"}, {"no-such-provider"}, {"claude", "extra"}, {"--watch"}} {
		if _, err := usageSelector(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if !slices.Contains(a.completionCandidatesFor([]string{"usage"}, "claude@"), "claude@work") {
		t.Fatal("credential completion missing")
	}
}

func TestUsageDiscoversPrivateRootsAndKeepsAmbiguityUnknown(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	state := t.TempDir()
	usageFixtureFile(t, filepath.Join(state, "acp", "session", "codex", "profiles", "retired", "sessions", "x.jsonl"), "fixture")
	rows, partial := a.usageCredentials([]string{"codex"}, state)
	if partial || len(rows) != 2 || rows[0].account != "retired" || len(rows[0].paths) != 1 {
		t.Fatalf("private discovery=%+v partial=%v", rows, partial)
	}
	e := agents.UsageEvent{ID: "same", Model: "gpt-6.1-sol", Time: time.Now(), Input: 100, Output: 10, WriteKnown: true, ContextKnown: true}
	rows = []usageCredential{
		{provider: "codex", account: "a", history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{e}}},
		{provider: "codex", account: "b", history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{e}}},
		{provider: "codex", account: "Unattributed ACP", shared: true},
	}
	rows = deduplicateUsageCredentials(rows)
	ag, _ := agents.Get("codex")
	for i := range rows {
		rows[i].value = agents.ValueUsageHistory(rows[i].history, time.Now(), ag.Usage().Price)
	}
	if rows[0].value.Available || rows[1].value.Available || rows[2].value.Priced != 1 {
		t.Fatalf("ambiguous history fabricated zero or double charged: %+v", rows)
	}
	var out bytes.Buffer
	renderUsage(&out, ui.Palette{}, 100, time.Now(), []string{"codex"}, rows[:1], true)
	if strings.Contains(out.String(), "≈$0") || !strings.Contains(out.String(), "attribution is ambiguous") {
		t.Fatalf("ambiguous selected credential: %s", out.String())
	}
}

func TestUsageRenderingIndependentFailuresAndReset(t *testing.T) {
	now := time.Now()
	used := 100.0
	rows := []usageCredential{
		{provider: "codex", account: "work", value: agents.UsageValue{Available: true, USD: 12.345, Priced: 1, Unpriced: 2, Partial: true}, quota: agents.UsageQuota{Plan: "Pro", Buckets: []agents.UsageBucket{{Name: "5-hour", Used: &used, Reset: now.Add(38 * time.Minute)}}}},
		{provider: "codex", account: "personal", quotaErr: agents.ErrUsageSignIn},
	}
	var wide, narrow bytes.Buffer
	renderUsage(&wide, ui.Palette{}, 120, now, []string{"codex"}, rows, true)
	renderUsage(&narrow, ui.Palette{}, 30, now, []string{"codex"}, rows, true)
	for _, wanted := range []string{"work · Pro", "≈$12.35 · partial history · 2 unpriced events", "100% used", "resets in 38m", "coop login codex@personal", "no usable history"} {
		if !strings.Contains(wide.String(), wanted) {
			t.Fatalf("missing %q: %s", wanted, wide.String())
		}
	}
	if !strings.Contains(narrow.String(), "    5-hour\n      ") {
		t.Fatalf("narrow facts did not stack: %s", narrow.String())
	}
	for _, line := range strings.Split(narrow.String(), "\n") {
		if strings.HasPrefix(line, " ") && utf8.RuneCountInString(line) > 30 {
			t.Fatalf("narrow credential fact overflows: %q", line)
		}
	}
	if usageResetLabel(time.Time{}, now) != "reset unknown" {
		t.Fatal("missing reset invented")
	}
}

func TestUsageSummaryKeepsBarsAndOnlyUsefulFacts(t *testing.T) {
	now := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.Local)
	zero, full := 0.0, 100.0
	blocked := false
	reset := now.Add(7 * 24 * time.Hour)
	rows := []usageCredential{
		{provider: "claude", account: "personal", unpricedTurns: 3,
			value: agents.UsageValue{Available: true, Priced: 1, USD: 406.48, Partial: true, Approximate: true, Unpriced: 22},
			quota: agents.UsageQuota{Plan: "max", Buckets: []agents.UsageBucket{
				{Name: "5-hour", Used: &zero},
				{Name: "Weekly", Used: &zero, Reset: reset},
				{Name: "Fable", Used: &zero, Reset: reset},
				{Name: "Extra usage", Note: "disabled"},
			}}},
		{provider: "codex", account: "emisar",
			quota: agents.UsageQuota{Buckets: []agents.UsageBucket{
				{Name: "Weekly", Used: &full, Available: &blocked},
				{Name: "gpt-reserve · Weekly", Used: &zero, Reset: reset},
				{Name: "Credits", Remaining: "60412.0160080000"},
			}}},
		{provider: "codex", account: "personal",
			quota: agents.UsageQuota{Buckets: []agents.UsageBucket{{Name: "Weekly", Used: &full}}}},
		{provider: "codex", account: "Unattributed ACP", shared: true,
			value: agents.UsageValue{Available: true, Priced: 1, USD: 0.82, Approximate: true}},
		{provider: "gemini", account: "api-key", quota: agents.UsageQuota{Note: "Limits unavailable for API-key authentication"}},
		{provider: "gemini", account: "vertex", quota: agents.UsageQuota{Note: "Limits unavailable for Vertex authentication"}},
	}
	var out bytes.Buffer
	renderUsage(&out, ui.Palette{}, 120, now, []string{"claude", "codex", "gemini"}, rows, false)
	text := out.String()
	for _, wanted := range []string{"  personal\n", "░░░░░░░░░░    0% used", "██████████  100% used · blocked", "Fable", "gpt-reserve · Weekly", "60412.02 remaining", "30-day API estimate  ≈$406.48", "Unassigned editor usage", "≈$0.82"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("missing %q:\n%s", wanted, text)
		}
	}
	for _, unwanted := range []string{" · default", " · max", "partial history", "approximate token tariff", "unpriced events", "retained turns", "Extra usage", "reset unknown", "usage unknown", "not billing", "Based on retained", "Reset times are local", "Unattributed ACP"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("summary contains %q:\n%s", unwanted, text)
		}
	}
	if strings.Index(text, "Weekly") > strings.Index(text, "30-day API estimate") {
		t.Fatalf("estimate precedes limits:\n%s", text)
	}
	if strings.Count(text, "resets Oct 8, 12:00") != 2 {
		t.Fatalf("inactive Fable repeats the weekly reset:\n%s", text)
	}
	barColumn := -1
	for _, line := range strings.Split(text, "\n") {
		if index := strings.IndexAny(line, "█░"); index >= 0 {
			column := utf8.RuneCountInString(line[:index])
			if barColumn >= 0 && column != barColumn {
				t.Fatalf("bars shifted between accounts: %s", line)
			}
			barColumn = column
		}
		if strings.Contains(line, "gpt-reserve") && strings.Contains(line, "blocked") {
			t.Fatalf("one exhausted bucket blocked the reserve: %s", line)
		}
	}
	gemini := text[strings.Index(text, "Gemini\n"):]
	if strings.Count(gemini, "    Limits ") != 2 ||
		!strings.Contains(gemini, "Limits unavailable for API-key authentication") ||
		!strings.Contains(gemini, "Limits unavailable for Vertex authentication") {
		t.Fatalf("Gemini repeats or obscures the unavailable reason:\n%s", gemini)
	}
	t.Log("\n" + text)

	out.Reset()
	renderUsage(&out, ui.Palette{}, 120, now, []string{"claude"}, rows[:1], true)
	for _, wanted := range []string{"partial history", "approximate token tariff", "22 unpriced events", "3 retained turns", "Extra usage", "disabled"} {
		if !strings.Contains(out.String(), wanted) {
			t.Fatalf("account detail missing %q:\n%s", wanted, out.String())
		}
	}
	for _, unwanted := range []string{" · default", "not billing", "Based on retained", "Reset times are local"} {
		if strings.Contains(out.String(), unwanted) {
			t.Fatalf("account detail contains %q:\n%s", unwanted, out.String())
		}
	}
}
