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
		accounts []string
	}{
		{"claude", "claude", "", nil},
		{"claude:opus-4.8", "claude", "opus-4.8", nil},
		{"claude@work", "claude", "", []string{"work"}},
		{"claude:opus-4.8@work", "claude", "opus-4.8", []string{"work"}},
		{"claude:opus@work,personal", "claude", "opus", []string{"work", "personal"}},
		{"claude@work,personal", "claude", "", []string{"work", "personal"}},
		{"codex:gpt-5.5", "codex", "gpt-5.5", nil},
		{"  gemini:gemini-2.5-pro @ acct ", "gemini", "gemini-2.5-pro", []string{"acct"}}, // trimmed
	}
	for _, c := range ok {
		got, err := ParseTarget(c.in)
		if err != nil {
			t.Errorf("ParseTarget(%q) errored: %v", c.in, err)
			continue
		}
		if got.Provider != c.provider || got.Model != c.model || strings.Join(got.Accounts, ",") != strings.Join(c.accounts, ",") {
			t.Errorf("ParseTarget(%q) = %+v, want provider=%q model=%q accounts=%v", c.in, got, c.provider, c.model, c.accounts)
		}
	}

	bad := map[string]string{
		"":                "empty target",
		"gpt":             "unknown provider", // not registered
		"nope:opus":       "unknown provider",
		"claude:":         "empty model",
		"claude@":         "empty account",
		"claude@work,":    "empty account",
		"claude@a@b":      "more than one @",
		"claude:a:b":      "no ':'", // model can't contain ':'
		"claude:opus@a:b": "no ':'", // account can't contain ':'
		"claude:op us":    "no ':' '@' or spaces",
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
	for _, s := range []string{"claude", "claude:opus-4.8", "claude@work", "claude:opus@work,personal"} {
		got, err := ParseTarget(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if got.String() != s {
			t.Errorf("round-trip %q -> %q", s, got.String())
		}
	}
}
