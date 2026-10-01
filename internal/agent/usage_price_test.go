package agent

import (
	"math"
	"testing"
	"time"
)

func TestUsageTokenValue(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		provider, model                  string
		input, read, write, hour, output int64
		expected                         float64
	}{
		{"claude", "claude-sonnet-4-6", 20, 100, 10, 20, 40, .0008475},
		{"claude", "claude-haiku-4-5-20251001", 20, 100, 10, 20, 40, .0002825},
		{"codex", "gpt-6.1-sol", 0, 40, 60, 0, 10, .000254},
		{"gemini", "gemini-2.5-pro", 40, 60, 0, 0, 25, .0003075},
		{"gemini", "gemini-3.1-pro-preview-customtools", 40, 60, 0, 0, 25, .000392},
		{"gemini", "gemini-3.8-flash", 40, 60, 0, 0, 25, .00012825},
		{"gemini", "gemini-3.7-flash", 40, 60, 0, 0, 25, .00012825},
		{"gemini", "gemini-3.6-flash", 40, 60, 0, 0, 25, .00012825},
		{"grok", "grok-4.3", 300, 600, 100, 0, 200, .00112},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			ag, _ := Get(tc.provider)
			event := UsageEvent{ID: "r", Model: tc.model, Time: now.Add(-time.Hour), Input: tc.input, Read: tc.read, Write: tc.write, Write1h: tc.hour, Output: tc.output, WriteKnown: true, ContextKnown: true}
			old := event
			old.ID = "old"
			old.Time = now.Add(-31 * 24 * time.Hour)
			future := event
			future.ID = "future"
			future.Time = now.Add(time.Hour)
			value := ValueUsageHistory(UsageHistory{Available: true, Events: []UsageEvent{old, event, event, future}}, now, ag.Usage().Price)
			if value.Priced != 1 || value.Unpriced != 0 || math.Abs(value.USD-tc.expected) > 1e-12 {
				t.Fatalf("value=%+v expected=%v", value, tc.expected)
			}
			event.Model = "unknown-model"
			value = ValueUsageHistory(UsageHistory{Available: true, Events: []UsageEvent{event}}, now, ag.Usage().Price)
			if value.Priced != 0 || value.Unpriced != 1 {
				t.Fatalf("unknown model became free: %+v", value)
			}
		})
	}
	for _, tc := range []struct {
		provider, model string
		input           int64
		expected        float64
	}{
		{"codex", "gpt-6.1-sol", 272000, .554},
		{"codex", "gpt-6.1-sol", 272001, 1.103004},
		{"gemini", "gemini-2.5-pro", 200000, .26},
		{"gemini", "gemini-2.5-pro", 200001, .5150025},
		{"gemini", "gemini-3.1-pro-preview-customtools", 200000, .412},
		{"gemini", "gemini-3.1-pro-preview-customtools", 200001, .818004},
		{"grok", "grok-4.3", 200000, .505},
	} {
		ag, _ := Get(tc.provider)
		event := UsageEvent{ID: "r", Model: tc.model, Time: now, Input: tc.input, Output: 1000, ContextKnown: true, WriteKnown: true}
		actual, ok := ag.Usage().Price(event)
		if !ok || math.Abs(actual-tc.expected) > 1e-12 {
			t.Fatalf("%s input=%d value=%v priced=%v expected=%v", tc.model, tc.input, actual, ok, tc.expected)
		}
	}
	unknownTTL := UsageEvent{ID: "r", Model: "claude-sonnet-4-6", Time: now, UnknownWrite: 20}
	if _, ok := claudeUsagePrice(unknownTTL); ok {
		t.Fatal("unknown cache TTL was priced as five minutes")
	}
	missingWrites := UsageEvent{ID: "r", Model: "gpt-6.1-sol", Time: now, Input: 100}
	if _, ok := codexUsagePrice(missingWrites); ok {
		t.Fatal("missing write category became zero")
	}
	unknownContext := UsageEvent{ID: "r", Model: "grok-4.3", Time: now, Input: 300000, Output: 1000}
	if _, ok := grokUsagePrice(unknownContext); ok {
		t.Fatal("multi-call turn selected a per-request context tariff")
	}
	if ValueUsageHistory(UsageHistory{}, now, codexUsagePrice).Available {
		t.Fatal("missing history became a known zero")
	}
	if _, ok := grokUsagePrice(UsageEvent{ID: "r", Model: "grok-4.6-build", Time: now, Input: 100, ContextKnown: true}); ok {
		t.Fatal("unverified native build model was mapped to public API pricing")
	}
}
