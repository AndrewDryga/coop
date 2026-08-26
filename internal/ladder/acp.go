package ladder

import (
	"encoding/json"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// ACPRateSignals returns the structured limit markers to match for a session led by
// lead: the adapter's own (each owns its wire format — see Agent.ACPRateLimitSignals),
// or, for an unknown lead, the union of every adapter's so no provider's limit goes
// unrecognized.
func ACPRateSignals(lead string) []agents.ACPSignal {
	if a, ok := agents.Get(lead); ok {
		return a.ACPRateLimitSignals()
	}
	var all []agents.ACPSignal
	for _, n := range agents.Names() {
		if a, ok := agents.Get(n); ok {
			all = append(all, a.ACPRateLimitSignals()...)
		}
	}
	return all
}

// ACPErrorLimitHint classifies a JSON-RPC error: prose detection (shared DetectLimit)
// plus the adapter-owned structured signals. It carries no provider constants itself —
// a new agent brings its markers via ACPRateLimitSignals.
func ACPErrorLimitHint(raw json.RawMessage, now time.Time, signals []agents.ACPSignal) LimitHint {
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &msg)
	hint := DetectLimit(msg.Message, now)

	var v any
	if json.Unmarshal(raw, &v) != nil {
		return hint
	}
	structuredRate, structuredOutput := false, false
	var proseReset time.Time
	WalkJSONStrings(v, "", func(key, value string) {
		k := CompactJSONName(key)
		vc := CompactJSONName(value)
		for _, s := range signals {
			if (s.Key == "" || CompactJSONName(s.Key) == k) && CompactJSONName(s.Value) == vc {
				structuredRate = true
			}
		}
		// The output-token axis is deliberately SHARED, not per-agent: stopReason is the
		// ACP-protocol stop-reason field, finishReason the common upstream-API leak, and
		// length/MAX_TOKENS spell "output budget exhausted" across providers.
		if (k == "finishreason" || k == "stopreason") && (vc == "length" || vc == "maxtokens") {
			structuredOutput = true
		}
		// The reset time often hides in a NESTED string: codex-acp's top-level message is a
		// generic "Internal error" while the human notice — "You've hit your usage limit. …
		// try again at 4:28 PM." — rides in data.message. Mine every string for a stated
		// reset (earliest wins) so the wait targets it instead of the 5-minute default.
		if h := DetectLimit(value, now); h.Limited && !h.OutputLimited && !h.ResetAt.IsZero() {
			if proseReset.IsZero() || h.ResetAt.Before(proseReset) {
				proseReset = h.ResetAt
			}
		}
	})
	if structuredRate {
		hint.Limited = true
		hint.OutputLimited = false
	} else if structuredOutput {
		hint.Limited = true
		hint.OutputLimited = true
	}
	if hint.Limited && !hint.OutputLimited && hint.ResetAt.IsZero() {
		hint.ResetAt = proseReset
	}
	return hint
}

// WalkJSONStrings visits every string leaf of a decoded JSON value with the key it hung off.
// Exported because the ACP wire is matched the same way outside this package — an adapter's
// authentication verdict and its model-restart suggestion are the same shape of hunt as a limit
// signal — and one walker in the tree beats a copy per caller.
func WalkJSONStrings(v any, key string, visit func(string, string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			WalkJSONStrings(child, k, visit)
		}
	case []any:
		for _, child := range x {
			WalkJSONStrings(child, key, visit)
		}
	case string:
		visit(key, x)
	}
}

// CompactJSONName folds a JSON key or value to its comparable form, so snake_case, kebab-case,
// spaced, and camelCase spellings of the same marker all match.
func CompactJSONName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}
