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

func TestUsageAlignsEveryFactAndQuietsTheTotal(t *testing.T) {
	now := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.Local)
	zero, used, full := 0.0, 26.0, 100.0
	allowed := false
	rows := []usageCredential{
		{provider: "codex", account: "emisar",
			value: agents.UsageValue{Available: true, Priced: 1, USD: 54.82},
			quota: agents.UsageQuota{Buckets: []agents.UsageBucket{
				{Name: "Weekly", Used: &full, Available: &allowed},
				{Name: "gpt-reserve · Weekly", Used: &zero},
				{Name: "Credits", Remaining: "62500"},
			}}},
		{provider: "codex", account: "work", quotaErr: agents.ErrUsageSignIn},
		{provider: "codex", account: "Unattributed ACP", shared: true,
			value: agents.UsageValue{Available: true, Priced: 1, USD: 0.82}},
		{provider: "gemini", account: "blitz_ai_studio", quota: agents.UsageQuota{Auth: "API key"},
			value: agents.UsageValue{Available: true, Priced: 1, USD: 0.48}},
		{provider: "gemini", account: "personal", quota: agents.UsageQuota{Auth: "API key"},
			value: agents.UsageValue{Available: true, Priced: 1, USD: 2.62}},
		{provider: "grok", account: "default",
			value: agents.UsageValue{Available: true, Unpriced: 3},
			quota: agents.UsageQuota{Note: "shared credit pool", Buckets: []agents.UsageBucket{{Name: "Weekly credits", Used: &used}}}},
	}
	names := []string{"codex", "gemini", "grok"}
	var out bytes.Buffer
	renderUsage(&out, ui.Palette{}, 120, now, names, rows, false)
	text := out.String()

	// Codex: labels fit its longest label; balances, reasons and totals all start where
	// "100% used" starts, two spaces after the bar, and percentages right-align to it.
	labels := utf8.RuneCountInString("gpt-reserve · Weekly")
	value := labels + 18
	at := func(column int, text string) string { return strings.Repeat(" ", column) + text + "\n" }
	for _, wanted := range []string{
		"    Weekly                ██████████  100% used\n",
		"    gpt-reserve · Weekly  ░░░░░░░░░░    0% used",
		"    Credits" + at(value-11, "62500 remaining"),
		"    Limits" + at(value-10, "unavailable · sign-in required"),
		at(value, "Run: coop login codex@work"),
		at(value, "Σ≈$54.82"),
		"  Unassigned editor usage" + at(value-25, "Σ≈$0.82"),
		// Accounts with nothing but a total are one line each, stacked, with no explanation.
		"Gemini\n  blitz_ai_studio (API key)  Σ≈$0.48\n  personal (API key)" + at(29-20, "Σ≈$2.62"),
		// Grok fits its own label; its note rides the account line, and without API pricing it
		// has no total line at all.
		"  default · shared credit pool\n    Weekly credits  ███░░░░░░░   26% used\n",
	} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("missing aligned text %q:\n%s", wanted, text)
		}
	}
	for _, unwanted := range []string{"blocked", "limit reached", "Limits unavailable", "\n    shared credit pool", "30-day API estimate", "unpriced"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("summary contains %q:\n%s", unwanted, text)
		}
	}
	if !strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\n\n") {
		t.Fatalf("output must end on its last block without a trailing blank line: %q", text[len(text)-12:])
	}

	// Totals and account notes are quieter than the limits; bars keep their colours.
	var colored bytes.Buffer
	pal := ui.Colored()
	renderUsage(&colored, pal, 120, now, names, rows, false)
	for _, wanted := range []string{
		pal.Dim(strings.TrimSuffix(at(value, "Σ≈$54.82"), "\n")) + "\n",
		"  blitz_ai_studio (API key)  " + pal.Dim("Σ≈$0.48") + "\n",
		"  default" + pal.Dim(" · shared credit pool") + "\n",
		pal.Red("██████████") + "  100% used",
	} {
		if !strings.Contains(colored.String(), wanted) {
			t.Fatalf("missing styled text %q:\n%s", wanted, colored.String())
		}
	}

	// Account details keep the provider's flag, in words, for a limit but not for a balance.
	out.Reset()
	renderUsage(&out, ui.Palette{}, 120, now, []string{"codex"}, rows[:1], true)
	if !strings.Contains(out.String(), "100% used · limit reached") || strings.Count(out.String(), "limit reached") != 1 {
		t.Fatalf("account details lost or misapplied the limit flag:\n%s", out.String())
	}

	// A fact too wide for its columns stacks under its label instead of spilling past the edge.
	out.Reset()
	renderUsage(&out, ui.Palette{}, 40, now, []string{"codex"}, rows[:2], false)
	if !strings.Contains(out.String(), "    Limits\n      unavailable · sign-in required\n") {
		t.Fatalf("narrow limits fact did not stack:\n%s", out.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if utf8.RuneCountInString(line) > 40 {
			t.Fatalf("narrow summary overflows: %q", line)
		}
	}
}

func TestUsageResetLabelCountsDownAndNamesUTC(t *testing.T) {
	// The host zone must not leak into the label: 08:00 at UTC-6 is 14:00 UTC.
	now := time.Date(2026, time.October, 2, 8, 0, 0, 0, time.FixedZone("host", -6*60*60))
	for _, tc := range []struct {
		reset time.Time
		want  string
	}{
		{now.Add(38 * time.Minute), "resets in 38m (Oct 2, 14:38 UTC)"},
		{now.Add(4*time.Hour + 55*time.Minute), "resets in 4h 55m (Oct 2, 18:55 UTC)"},
		{now.Add(2*24*time.Hour + 9*time.Hour + 35*time.Minute), "resets in 2d 9h (Oct 4, 23:35 UTC)"},
		{now.Add(-time.Hour), "reset passed (Oct 2, 13:00 UTC)"},
		{now.AddDate(0, 3, 0), "resets in 92d 0h (Jan 2 2027, 14:00 UTC)"},
		{time.Time{}, "reset unknown"},
	} {
		if got := usageResetLabel(tc.reset, now); got != tc.want {
			t.Errorf("usageResetLabel(%v) = %q, want %q", tc.reset, got, tc.want)
		}
	}
}

func TestUsageWrapsValuesInTheirColumn(t *testing.T) {
	for _, tc := range []struct {
		value string
		room  int
		want  []string
	}{
		{"0% used", 20, []string{"0% used"}},
		{"100% used · resets in 2d 2h (Oct 4, 08:35 UTC)", 40, []string{"100% used", "resets in 2d 2h (Oct 4, 08:35 UTC)"}},
		{"100% used · resets in 2d 2h (Oct 4, 08:35 UTC)", 22, []string{"100% used", "resets in 2d 2h", "(Oct 4, 08:35 UTC)"}},
		{"unavailable · sign-in required", 12, []string{"unavailable", "sign-in", "required"}},
	} {
		if got := usageWrap(tc.value, tc.room); !slices.Equal(got, tc.want) {
			t.Errorf("usageWrap(%q, %d) = %q, want %q", tc.value, tc.room, got, tc.want)
		}
	}

	// On a narrower terminal a row keeps its columns and continues in the value column.
	now := time.Date(2026, time.October, 2, 6, 35, 0, 0, time.UTC)
	full := 100.0
	rows := []usageCredential{{provider: "codex", account: "personal",
		value: agents.UsageValue{Available: true, Priced: 1, USD: 0.4},
		quota: agents.UsageQuota{Buckets: []agents.UsageBucket{
			{Name: "Weekly", Used: &full, Reset: now.Add(50 * time.Hour)},
			{Name: "Credits", Remaining: "60149.85"},
		}}}}
	var out bytes.Buffer
	renderUsage(&out, ui.Palette{}, 44, now, []string{"codex"}, rows, false)
	value := strings.Repeat(" ", 4+len("Credits")+2+12)
	want := "  personal\n" +
		"    Weekly   ██████████  100% used\n" +
		value + "resets in 2d 2h\n" +
		value + "(Oct 4, 08:35 UTC)\n" +
		"    Credits" + strings.Repeat(" ", 2+12) + "60149.85 remaining\n" +
		value + "Σ≈$0.40\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Fatalf("narrow row lost its columns:\n%s\nwant suffix:\n%s", out.String(), want)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if utf8.RuneCountInString(line) > 44 {
			t.Fatalf("narrow row overflows: %q", line)
		}
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
		{provider: "gemini", account: "api-key", quota: agents.UsageQuota{Auth: "API key"}},
		{provider: "gemini", account: "vertex", quota: agents.UsageQuota{Auth: "Vertex"}},
	}
	var out bytes.Buffer
	renderUsage(&out, ui.Palette{}, 120, now, []string{"claude", "codex", "gemini"}, rows, false)
	text := out.String()
	for _, wanted := range []string{"  personal\n", "░░░░░░░░░░    0% used", "██████████  100% used", "Fable", "gpt-reserve · Weekly", "60412.02 remaining", "Σ≈$406.48", "Unassigned editor usage", "Σ≈$0.82"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("missing %q:\n%s", wanted, text)
		}
	}
	for _, unwanted := range []string{" · default", " · max", "partial history", "approximate token tariff", "unpriced events", "retained turns", "Extra usage", "reset unknown", "usage unknown", "not billing", "Based on retained", "Reset times are local", "Unattributed ACP", "blocked", "Σ unavailable", "Σ unpriced"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("summary contains %q:\n%s", unwanted, text)
		}
	}
	if strings.Index(text, "Weekly") > strings.Index(text, "Σ≈$") {
		t.Fatalf("estimate precedes limits:\n%s", text)
	}
	if strings.Count(text, usageResetLabel(reset, now)) != 2 {
		t.Fatalf("inactive Fable repeats the weekly reset:\n%s", text)
	}
	// Within a provider every bar starts in one column, each total starts where "100%" would,
	// two spaces after the bar, and every percentage ends in one column, so "used" lines up; a
	// new provider measures its own columns.
	barColumn := -1
	for _, line := range strings.Split(text, "\n") {
		if line != "" && !strings.HasPrefix(line, " ") {
			barColumn = -1
		}
		if index := strings.IndexAny(line, "█░"); index >= 0 {
			column := utf8.RuneCountInString(line[:index])
			if barColumn >= 0 && column != barColumn {
				t.Fatalf("bars shifted between accounts: %s", line)
			}
			barColumn = column
		}
		if index := strings.Index(line, "Σ"); index >= 0 && barColumn >= 0 && utf8.RuneCountInString(line[:index]) != barColumn+12 {
			t.Fatalf("total does not start with the percentage: %q", line)
		}
		if index := strings.Index(line, "% used"); index >= 0 && utf8.RuneCountInString(line[:index]) != barColumn+15 {
			t.Fatalf("percentage is not right-aligned: %q", line)
		}
	}
	gemini := text[strings.Index(text, "Gemini\n"):]
	if strings.Contains(gemini, "Limits") || strings.Contains(gemini, "Σ") || !strings.HasSuffix(gemini, "Gemini\n  api-key (API key)\n  vertex (Vertex)\n") {
		t.Fatalf("Gemini sign-ins without limits or pricing are not one plain line each:\n%s", gemini)
	}
	t.Log("\n" + text)

	out.Reset()
	renderUsage(&out, ui.Palette{}, 30, now, []string{"claude"}, rows[:1], false)
	if !strings.Contains(out.String(), "    Σ≈$406.48\n") {
		t.Fatalf("narrow total did not use the compact fallback:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "    5-hour\n      ░░░░░░░░░░\n        0% used\n") {
		t.Fatalf("stacked percentage lost its alignment:\n%s", out.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if utf8.RuneCountInString(line) > 30 {
			t.Fatalf("narrow summary overflows: %q", line)
		}
	}

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
