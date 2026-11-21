package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
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
		{"gemini:pro/high", "gemini", "pro", "high", nil},                                     // gemini thinks at low or high
		{"gemini/low@work", "gemini", "", "low", []string{"work"}},
	}
	fast := []string{"codex:gpt-6.2-sol+fast", "codex:gpt-6.2-sol/high+fast@work", "claude:opus/xhigh+fast@work", "claude+fast"}
	for _, in := range fast {
		got, err := ParseTarget(in)
		if err != nil || !got.Fast {
			t.Errorf("ParseTarget(%q) = %+v, %v; want fast mode", in, got, err)
		}
		if got.String() != in {
			t.Errorf("round-trip %q -> %q", in, got.String())
		}
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
		"":                            "Missing agent target",
		"gpt":                         "Unknown agent", // not registered
		"nope:opus":                   "Unknown agent",
		"claude:":                     "Add a model after",
		"claude@":                     "Add an account after",
		"claude@work,":                "Add an account after",
		"claude@a@b":                  `Use one "@" before the account name`,
		"claude:opus/high/low":        `Use one "/" before the reasoning effort`,
		"clade:opus/high/low":         "Unknown agent",               // a mistyped agent wins over the second "/"
		"claude:a:b":                  "A model name cannot contain", // model can't contain ':'
		"claude:opus@a:b":             "Invalid account name",        // account can't contain ':'
		"claude:op us":                "A model name cannot contain",
		"claude:op\x1b[2Jus":          "control or invisible characters",
		"codex:gpt\u202e5":            "control or invisible characters",
		"codex:gpt\u200b5/high":       "control or invisible characters",
		"claude:op\xffus":             "control or invisible characters",
		"claude:opus/":                "Add a reasoning effort after",    // '/' with nothing after
		"claude:opus/HIGH":            "lowercase letters",               // effort is lowercase letters
		"claude:opus@work/high":       "Invalid account name",            // effort must precede the account
		"gemini/medium":               "Gemini takes effort low or high", // Gemini 3 has no medium level
		"gemini:gemini-2.5-pro/xhigh": "Gemini takes effort low or high",
		"gemini:gemini-9/high":        "run gemini-9 without an effort", // no thinking base reaches it
		"gemini:pro/high+fast":        "Gemini has no fast mode",
		"grok+fast":                   "Grok has no fast mode",
		"codex:gpt-6.2-sol+turbo":     `Write fast mode as "+fast"`,
		"codex:gpt-6.2-sol+fast/high": `Write fast mode as "+fast"`, // +fast follows the effort
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

func TestClaudeHaikuEffortIsRefused(t *testing.T) {
	for _, model := range []string{"claude-haiku-4-5", "claude-haiku-4-5-20251001"} {
		for _, effort := range []string{"low", "high"} {
			if _, err := ParseTarget("claude:" + model + "/" + effort + "@personal"); err == nil || !strings.Contains(err.Error(), "without /"+effort) {
				t.Errorf("%s/%s = %v, want actionable unsupported effort refusal", model, effort, err)
			}
		}
		if _, err := ParseTarget("claude:" + model + "@personal"); err != nil {
			t.Errorf("%s without effort was refused: %v", model, err)
		}
	}
	// The haiku alias is Haiku 5.5 since the pinned Claude Code made it the default Haiku, and
	// Haiku 5.5 takes an effort like Opus and Sonnet.
	for _, model := range []string{"haiku", "claude-haiku-5-5", "opus", "sonnet", "claude-fable-5-2", "claude-haiku-9", "custom-haiku", "claude-haiku-4-5-custom"} {
		if _, err := ParseTarget("claude:" + model + "/low@personal"); err != nil {
			t.Errorf("supporting or unknown model %s was refused: %v", model, err)
		}
	}
}

// Claude Code only warns about an effort it does not know and runs at its default, so Coop refuses
// one before launch, naming the levels; every level the pinned client lists still works.
func TestClaudeUnknownEffortIsRefused(t *testing.T) {
	for _, target := range []string{"claude:opus/bogus", "claude/mxa", "claude:haiku/minimal@work"} {
		if _, err := ParseTarget(target); err == nil || !strings.Contains(err.Error(), "low, medium, high, xhigh, or max") {
			t.Errorf("%s = %v, want refused naming the levels", target, err)
		}
	}
	for _, effort := range claudeEfforts {
		for _, target := range []string{"claude/" + effort, "claude:opus/" + effort, "claude:claude-fable-5-2/" + effort} {
			if _, err := ParseTarget(target); err != nil {
				t.Errorf("%s was refused: %v", target, err)
			}
		}
	}
	// Agents that hand the level on unchecked keep doing so, so a provider's new level works.
	for _, target := range []string{"codex:gpt-6.2-sol/ultra", "codex/minimal", "grok:grok-4.7/high"} {
		if _, err := ParseTarget(target); err != nil {
			t.Errorf("%s was refused: %v", target, err)
		}
	}
}

// COOP_<AGENT>_MODEL holds a target's model[/effort] slots, so config refuses what a target would,
// with the target's own reason, the setting's name and where it was set; loading never refuses it.
func TestModelSettingIsCheckedLikeATarget(t *testing.T) {
	cleanCmdEnv(t)
	empty := filepath.Join(t.TempDir(), "coop.conf")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONF", empty)
	load := func() *config.Config {
		t.Helper()
		cfg, err := config.Load() // never refused here: a bad value stops only a launch that reads it
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		return cfg
	}
	for _, ok := range []string{"opus", "opus/high", " fable/max ", "/high", "claude-haiku-4-5"} {
		t.Setenv("COOP_CLAUDE_MODEL", ok)
		if err := load().CheckModelSetting("claude"); err != nil {
			t.Errorf("COOP_CLAUDE_MODEL=%q was refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"bad/effort/x", "opus/", "op us", "opus/HIGH", "claude-haiku-4-5/low"} {
		t.Setenv("COOP_CLAUDE_MODEL", bad)
		err := load().CheckModelSetting("claude")
		var te *TargetError
		_, terr := ParseTarget("claude:" + bad)
		if !errors.As(terr, &te) {
			t.Fatalf("claude:%s as a target = %v, want refused", bad, terr)
		}
		if err == nil || !strings.Contains(err.Error(), "COOP_CLAUDE_MODEL") || !strings.Contains(err.Error(), te.Cause) ||
			!strings.Contains(err.Error(), "Set in the environment") {
			t.Errorf("COOP_CLAUDE_MODEL=%q = %v, want refused naming the setting, the environment and %q", bad, err, te.Cause)
		}
	}
	t.Setenv("COOP_CLAUDE_MODEL", "opus@work")
	if err := load().CheckModelSetting("claude"); err == nil || !strings.Contains(err.Error(), "account") {
		t.Errorf("COOP_CLAUDE_MODEL with an account = %v, want refused", err)
	}
	t.Setenv("COOP_CLAUDE_MODEL", "opus")
	t.Setenv("COOP_GEMINI_MODEL", "gemini-2.5-pro/xhigh")
	cfg := load()
	if err := cfg.CheckModelSetting("claude"); err != nil {
		t.Errorf("a bad COOP_GEMINI_MODEL stopped claude: %v", err)
	}
	if err := cfg.CheckModelSetting("gemini"); err == nil || !strings.Contains(err.Error(), "COOP_GEMINI_MODEL") {
		t.Errorf("COOP_GEMINI_MODEL with xhigh = %v, want refused", err)
	}
	os.Unsetenv("COOP_GEMINI_MODEL") // restored by t.Setenv's cleanup
	conf := filepath.Join(t.TempDir(), "coop.conf")
	if err := os.WriteFile(conf, []byte("COOP_CLAUDE_MODEL=bad/effort/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONF", conf)
	if err := load().CheckModelSetting("claude"); err != nil { // the environment's opus shadows the file
		t.Errorf("a shadowed coop.conf value was refused: %v", err)
	}
	os.Unsetenv("COOP_CLAUDE_MODEL")
	if err := load().CheckModelSetting("claude"); err == nil || !strings.Contains(err.Error(), "Set in "+conf) {
		t.Errorf("coop.conf COOP_CLAUDE_MODEL=bad/effort/x = %v, want refused naming %s", err, conf)
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

func TestLoginCommandQuotesUnsafeTarget(t *testing.T) {
	if got := LoginCommand("claude@personal_backup"); got != "coop login claude@personal_backup" {
		t.Fatalf("safe login command = %q", got)
	}
	if got := LoginCommand("claude@work; $(touch nope)"); got != "coop login 'claude@work; $(touch nope)'" {
		t.Fatalf("unsafe login command = %q", got)
	}
	if got := LoginCommand("claude@founder's"); got != `coop login 'claude@founder'"'"'s'` {
		t.Fatalf("quoted login command = %q", got)
	}
	if got := LoginCommand("claude@line\nnext"); got != `coop login $'claude@line\nnext'` {
		t.Fatalf("control-character login command = %q", got)
	}
	if got := DisplayTarget("claude@line\nnext"); got != `"claude@line\nnext"` {
		t.Fatalf("control-character target display = %q", got)
	}
}
