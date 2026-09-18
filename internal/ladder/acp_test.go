package ladder

import (
	"encoding/json"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// TestACPErrorLimitHintSignalDriven pins the classifier's contract: it matches whatever
// signals it is HANDED (compactly, key-pinned when a key is given) and carries no
// provider constants of its own — plus the shared output-token axis that needs no
// signals at all, and rate winning over output when both appear.
func TestACPErrorLimitHintSignalDriven(t *testing.T) {
	now := time.Now()
	sig := []agents.ACPSignal{{Value: "quotaBlown"}, {Key: "reason", Value: "too_fast"}}
	if h := ACPErrorLimitHint(json.RawMessage(`{"message":"nope","data":{"x":"quotaBlown"}}`), now, sig); !h.Limited || h.OutputLimited {
		t.Errorf("any-key signal should classify as a rate limit, got %+v", h)
	}
	if h := ACPErrorLimitHint(json.RawMessage(`{"message":"nope","data":{"reason":"tooFast"}}`), now, sig); !h.Limited {
		t.Errorf("key-pinned signal should compact-match tooFast/too_fast, got %+v", h)
	}
	if h := ACPErrorLimitHint(json.RawMessage(`{"message":"nope","data":{"other":"too_fast"}}`), now, sig); h.Limited {
		t.Errorf("a key-pinned value under the wrong key must not match, got %+v", h)
	}
	if h := ACPErrorLimitHint(json.RawMessage(`{"message":"x","data":{"stopReason":"MAX_TOKENS"}}`), now, nil); !h.Limited || !h.OutputLimited {
		t.Errorf("the shared output axis needs no signals, got %+v", h)
	}
	both := json.RawMessage(`{"message":"x","data":{"stopReason":"MAX_TOKENS","y":"quotaBlown"}}`)
	if h := ACPErrorLimitHint(both, now, sig); !h.Limited || h.OutputLimited {
		t.Errorf("a structured rate signal outranks the output axis, got %+v", h)
	}
}

// TestACPErrorLimitHintNestedProseReset pins the codex-acp wire shape captured live on
// 2026-07-10: the JSON-RPC message is a generic "Internal error", and the human notice
// carrying the reset clock time rides in data.message. The classifier must mine the
// nested prose so the wait targets the stated reset, not the 5-minute default cooldown.
func TestACPErrorLimitHintNestedProseReset(t *testing.T) {
	now := time.Date(2026, 7, 10, 14, 27, 0, 0, time.Local)
	raw := json.RawMessage(`{"code":-32603,"message":"Internal error","data":{"message":"You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 4:28 PM.","codexErrorInfo":"usageLimitExceeded"}}`)
	h := ACPErrorLimitHint(raw, now, []agents.ACPSignal{{Value: "usageLimitExceeded"}})
	if !h.Limited || h.OutputLimited {
		t.Fatalf("captured codex limit error must classify as a rate limit, got %+v", h)
	}
	want := time.Date(2026, 7, 10, 16, 28, 0, 0, time.Local)
	if !h.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v (mined from data.message prose)", h.ResetAt, want)
	}
	// A nested reset never RE-classifies: without the structured signal or limit prose in
	// the top-level message, an ordinary error stays ordinary even when a nested string
	// parses as a full limit notice (echoed user content must not drive a rotation).
	plain := json.RawMessage(`{"code":-32603,"message":"boom","data":{"note":"You've hit your usage limit. Try again at 4:28 PM."}}`)
	if h := ACPErrorLimitHint(plain, now, nil); h.Limited || !h.ResetAt.IsZero() {
		t.Errorf("nested prose alone must not classify the error as limited, got %+v", h)
	}
}

// The pinned Grok ACP adapter's error shapes, captured by replaying each status at the client. Its
// rate-limit error (read from its message by the shared check) and its 402 status rotate; an
// authentication failure, a server error, and prose that merely mentions a limit or a status do not.
func TestGrokACPLimitSignalsAreTheClientsOwn(t *testing.T) {
	now := time.Now()
	signals := ACPRateSignals("grok")
	for _, c := range []struct {
		name, raw string
		limited   bool
	}{
		{"429 rate limited", `{"code":-32003,"message":"Rate limited","data":"API error (status 429 Too Many Requests): rate_limit_exceeded: Too many requests."}`, true},
		{"402 out of credits", `{"code":-32603,"message":"Internal error","data":{"message":"API error (status 402 Payment Required): insufficient_credits: You have run out of credits.","http_status":402}}`, true},
		{"401 authentication", `{"code":-32603,"message":"Internal error","data":{"message":"Auth recovery succeeded but 4 authenticated inference requests were still rejected (401); giving up after 3 retries.","http_status":401}}`, false},
		{"500 server error", `{"code":-32603,"message":"Internal error","data":{"message":"API error (status 500 Internal Server Error): internal_error: The server had an error processing your request.","http_status":500}}`, false},
		{"prose about limits", `{"code":-32603,"message":"Internal error","data":{"message":"the model wrote: rate limited, 402"}}`, false},
		{"a status that is not a quota", `{"code":-32603,"message":"Internal error","data":{"message":"API error (status 403 Forbidden)","http_status":403}}`, false},
	} {
		if h := ACPErrorLimitHint(json.RawMessage(c.raw), now, signals); h.Limited != c.limited || h.OutputLimited {
			t.Errorf("%s: limit hint = %+v, want limited=%v", c.name, h, c.limited)
		}
	}
}
