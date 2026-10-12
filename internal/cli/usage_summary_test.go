package cli

import (
	"bytes"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestUsageAggregateCoverageAndRounding(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  []usageCredential
		known bool
		label string
	}{
		{"round once", []usageCredential{
			{provider: "codex", value: agents.UsageValue{Available: true, Priced: 1, USD: .004}},
			{provider: "codex", value: agents.UsageValue{Available: true, Priced: 1, USD: .004}},
			{provider: "claude", value: agents.UsageValue{Available: true, Priced: 1, USD: 900}},
		}, true, "≈$0.01/30d"},
		{"missing", []usageCredential{{provider: "codex"}}, false, "≈$0.00/30d partial"},
		{"unpriced", []usageCredential{{provider: "codex", value: agents.UsageValue{Available: true, Unpriced: 1}}}, false, "≈$0.00/30d partial"},
		{"known zero", []usageCredential{{provider: "codex", value: agents.UsageValue{Available: true}}}, true, "≈$0.00/30d"},
		{"partial empty", []usageCredential{{provider: "codex", value: agents.UsageValue{Available: true, Partial: true}}}, false, "≈$0.00/30d partial"},
		{"empty editor omitted", []usageCredential{
			{provider: "codex", value: agents.UsageValue{Available: true, Priced: 1, USD: 2}},
			{provider: "codex", shared: true},
		}, true, "≈$2.00/30d"},
		{"partial editor scan", []usageCredential{
			{provider: "codex", value: agents.UsageValue{Available: true, Priced: 1, USD: 2}},
			{provider: "codex", shared: true, history: agents.UsageHistory{Partial: true}},
		}, true, "≈$2.00/30d partial"},
		{"partial priced", []usageCredential{{provider: "codex", value: agents.UsageValue{Available: true, Priced: 1, USD: 3, Partial: true, Unpriced: 2}}}, true, "≈$3.00/30d partial"},
		{"unknown account", []usageCredential{
			{provider: "codex", value: agents.UsageValue{Available: true, Priced: 1, USD: 3}},
			{provider: "codex", quotaErr: agents.ErrUsageSignIn},
		}, true, "≈$3.00/30d partial"},
		{"invalid", []usageCredential{{provider: "codex", value: agents.UsageValue{Available: true, Priced: 1, USD: math.NaN()}}}, false, "≈$0.00/30d partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sumUsage(tc.rows, "codex")
			if got.Known != tc.known || got.label() != tc.label {
				t.Fatalf("aggregate=%+v label=%q want known=%v label=%q", got, got.label(), tc.known, tc.label)
			}
			var out bytes.Buffer
			renderUsage(&out, ui.Palette{}, 100, time.Now(), []string{"codex"}, tc.rows, false)
			if strings.Contains(out.String(), "Total  ") != tc.known || strings.Contains(out.String(), "Subtotal  ") != tc.known {
				t.Fatalf("unknown aggregate fabricated or known aggregate omitted:\n%s", out.String())
			}
		})
	}
}

func TestUsageAggregatesDeduplicatedEditorHistoryOnce(t *testing.T) {
	now := time.Now()
	event := agents.UsageEvent{ID: "same", Model: "gpt-6.1-sol", Time: now, Input: 100, Output: 10, WriteKnown: true, ContextKnown: true}
	for _, ambiguous := range []bool{false, true} {
		rows := []usageCredential{
			{provider: "codex", account: "a", history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{event}}},
			{provider: "codex", shared: true, history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{event}}},
		}
		if ambiguous {
			rows = append(rows, usageCredential{provider: "codex", account: "b", history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{event}}})
		}
		rows = deduplicateUsageCredentials(rows)
		ag, _ := agents.Get("codex")
		for i := range rows {
			rows[i].value = agents.ValueUsageHistory(rows[i].history, now, ag.Usage().Price)
		}
		want, ok := ag.Usage().Price(event)
		got := sumUsage(rows, "codex")
		if !ok || !got.Known || got.USD != want || got.Partial != ambiguous {
			t.Fatalf("shared event counted incorrectly: ambiguous=%v sum=%+v want=%v", ambiguous, got, want)
		}
	}
}

func TestUsageResetCountExpiryAndGrayTiming(t *testing.T) {
	now := time.Date(2026, 10, 12, 19, 13, 0, 0, time.UTC)
	used, count := 14.0, int64(4)
	row := usageCredential{provider: "codex", account: "personal",
		value: agents.UsageValue{Available: true, Priced: 1, USD: 14.17},
		quota: agents.UsageQuota{Buckets: []agents.UsageBucket{
			{Name: "Weekly", Used: &used, Reset: now.Add(77 * time.Minute)},
			{Name: "Credits", Remaining: "58409.420000"},
		}, Resets: &agents.UsageResetCredits{Available: &count, Expirations: []time.Time{now.Add(48 * time.Hour)}}}}
	for _, width := range []int{0, 120, 80, 44, 30} {
		var plain, colored bytes.Buffer
		renderUsage(&plain, ui.Palette{}, width, now, []string{"codex"}, []usageCredential{row}, false)
		pal := ui.Colored()
		renderUsage(&colored, pal, width, now, []string{"codex"}, []usageCredential{row}, false)
		text := plain.String()
		if unstyled := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(colored.String(), ""); unstyled != text {
			t.Fatalf("ANSI layout differs from plain width=%d:\n%s", width, unstyled)
		}
		if strings.Contains(text, " · resets") || strings.ContainsRune(text, '\x1b') || !strings.Contains(text, "4 available") || !strings.Contains(text, "58,409.42") || !strings.Contains(text, "Σ≈$14.17/30d") {
			t.Fatalf("bad compact usage width=%d:\n%s", width, text)
		}
		if !strings.Contains(colored.String(), pal.Bold("Total  ≈$14.17/30d")) {
			t.Fatal("grand total is not bold")
		}
		for _, line := range strings.Split(text, "\n") {
			if width > 0 && utf8.RuneCountInString(line) > width {
				t.Fatalf("width=%d overflow %q", width, line)
			}
		}
		if width == 120 && (!strings.Contains(colored.String(), "14% used"+pal.Faint("  resets in 1h 17m (Oct 12, 20:30 UTC)")) || !strings.Contains(text, "reported expiry in 2d 0h")) {
			t.Fatalf("timing is not separately gray and dot-free:\n%s", colored.String())
		}
		if width == 44 && !strings.Contains(colored.String(), pal.Faint("resets in 1h 17m")) {
			t.Fatalf("wrapped reset lost its gray styling:\n%s", colored.String())
		}
	}
	var out bytes.Buffer
	row.quota.Resets = &agents.UsageResetCredits{}
	renderUsage(&out, ui.Palette{}, 120, now, []string{"codex"}, []usageCredential{row}, true)
	if !strings.Contains(out.String(), "Resets") || !strings.Contains(out.String(), "unavailable") || strings.Contains(out.String(), "0 available") || strings.Contains(out.String(), "Subtotal") || strings.Contains(out.String(), "Total  ") {
		t.Fatalf("unknown reset count or exact-account detail view incorrect:\n%s", out.String())
	}
	count = 0
	row.quota.Resets = &agents.UsageResetCredits{Available: &count}
	out.Reset()
	renderUsage(&out, ui.Palette{}, 120, now, []string{"codex"}, []usageCredential{row}, false)
	if !strings.Contains(out.String(), "0 available") || strings.Contains(out.String(), "reported expiry") {
		t.Fatalf("known zero resets did not stay zero:\n%s", out.String())
	}
	count = 4
	row.quota.Resets = &agents.UsageResetCredits{Available: &count, Expirations: []time.Time{now.Add(48 * time.Hour)}}
	out.Reset()
	renderUsage(&out, ui.Palette{}, 120, now, []string{"codex"}, []usageCredential{row}, true)
	if !strings.Contains(out.String(), "Reported reset expiry") || !strings.Contains(out.String(), "in 2d 0h (Oct 14, 19:13 UTC)") {
		t.Fatalf("account details lost reported expiry:\n%s", out.String())
	}
}

func TestUsageGrandTotalFiltersProvidersAndMarksUnknownCoverage(t *testing.T) {
	rows := []usageCredential{
		{provider: "codex", value: agents.UsageValue{Available: true, Priced: 1, USD: 3}},
		{provider: "claude", quotaErr: agents.ErrUsageSignIn},
		{provider: "gemini", value: agents.UsageValue{Available: true, Priced: 1, USD: 900}},
	}
	for _, tc := range []struct {
		names []string
		want  string
	}{
		{[]string{"codex"}, "Total  ≈$3.00/30d\n"},
		{[]string{"codex", "claude"}, "Total  ≈$3.00/30d partial\n"},
	} {
		var out bytes.Buffer
		renderUsage(&out, ui.Palette{}, 120, time.Now(), tc.names, rows, false)
		if !strings.HasSuffix(out.String(), tc.want) || strings.Contains(out.String(), "$900") || strings.Contains(out.String(), "$903") {
			t.Fatalf("filtered total leaked another provider or hid unknown coverage:\n%s", out.String())
		}
	}
}
