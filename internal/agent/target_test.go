package agent

import (
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	ok := []struct {
		in       string
		provider string
		model    string
		effort   string
		accounts []string
	}{
		{"claude", "claude", "", "", nil},
		{"claude:opus-4.8", "claude", "opus-4.8", "", nil},
		{"claude@work", "claude", "", "", []string{"work"}},
		{"claude:opus-4.8@work", "claude", "opus-4.8", "", []string{"work"}},
		{"claude:opus@work,personal", "claude", "opus", "", []string{"work", "personal"}},
		{"claude@work,personal", "claude", "", "", []string{"work", "personal"}},
		{"codex:gpt-5.5", "codex", "gpt-5.5", "", nil},
		{"  gemini:gemini-2.5-pro @ acct ", "gemini", "gemini-2.5-pro", "", []string{"acct"}}, // trimmed
		{"claude:opus/xhigh", "claude", "opus", "xhigh", nil},                                 // model + effort
		{"codex/high", "codex", "", "high", nil},                                              // effort, CLI-default model
		{"codex:gpt-5.5/high@work", "codex", "gpt-5.5", "high", []string{"work"}},             // model + effort + account
	}
	for _, c := range ok {
		got, err := ParseTarget(c.in)
		if err != nil {
			t.Errorf("ParseTarget(%q) errored: %v", c.in, err)
			continue
		}
		if got.Provider != c.provider || got.Model != c.model || got.Effort != c.effort || strings.Join(got.Accounts, ",") != strings.Join(c.accounts, ",") {
			t.Errorf("ParseTarget(%q) = %+v, want provider=%q model=%q effort=%q accounts=%v", c.in, got, c.provider, c.model, c.effort, c.accounts)
		}
	}

	bad := map[string]string{
		"":                      "empty target",
		"gpt":                   "unknown provider", // not registered
		"nope:opus":             "unknown provider",
		"claude:":               "empty model",
		"claude@":               "empty account",
		"claude@work,":          "empty account",
		"claude@a@b":            "more than one @",
		"claude:a:b":            "no ':'", // model can't contain ':'
		"claude:opus@a:b":       "no ':'", // account can't contain ':'
		"claude:op us":          "no ':' '@' '/'",
		"claude:opus/":          "empty effort",                // '/' with nothing after
		"claude:opus/HIGH":      "invalid effort",              // effort is lowercase letters
		"claude:opus@work/high": "invalid account",             // effort must precede the account
		"gemini:pro/high":       "no reasoning-effort control", // gemini exposes none
	}
	for in, want := range bad {
		_, err := ParseTarget(in)
		if err == nil {
			t.Errorf("ParseTarget(%q) should error", in)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ParseTarget(%q) error = %q, want it to mention %q", in, err.Error(), want)
		}
	}
}

// String round-trips the wire form so config/messages/tests share one rendering.
func TestTargetString(t *testing.T) {
	for _, s := range []string{"claude", "claude:opus-4.8", "claude@work", "claude:opus@work,personal", "claude:opus/xhigh", "codex/high", "codex:gpt-5.5/high@work"} {
		got, err := ParseTarget(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if got.String() != s {
			t.Errorf("round-trip %q -> %q", s, got.String())
		}
	}
}
