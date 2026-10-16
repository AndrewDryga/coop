package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestUsageNativeOnlyAndExactAccountExclusion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("NO_COLOR", "1")
	data := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"native","message":{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20}}}`, time.Now().UTC().Format(time.RFC3339Nano))
	usageFixtureFile(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"), data)
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir(), RuntimeName: "must-not-run"}}
	out := captureStdout(t, func() {
		code, err := a.cmdUsage([]string{"claude"})
		if code != 0 || err != nil {
			t.Fatalf("%d %v", code, err)
		}
	})
	if !strings.Contains(out, "Unassigned local usage") || !strings.Contains(out, "Subtotal") || a.rtSet {
		t.Fatal(out)
	}
	if code, err := a.cmdUsage([]string{"claude@personal"}); code != 2 || err == nil {
		t.Fatalf("native history invented account: %d %v", code, err)
	}
}

func TestUsagePrunesCacheOnlyAfterCheckingEveryProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GEMINI_CLI_HOME", "GROK_HOME"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_COLOR", "1")
	data := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"native","message":{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20}}}`, time.Now().UTC().Format(time.RFC3339Nano))
	usageFixtureFile(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"), data)
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir(), RuntimeName: "must-not-run"}}
	// The cache of a history root no check reads any more, as after removing a credential.
	retired := filepath.Join(a.cfg.BoxHome, "usage-cache", strings.Repeat("0", 64))
	usageFixtureFile(t, filepath.Join(retired, strings.Repeat("1", 64)+".json"), "{}")
	check := func(args ...string) {
		t.Helper()
		captureStdout(t, func() {
			if code, err := a.cmdUsage(args); code != 0 || err != nil {
				t.Fatalf("usage %v = (%d, %v)", args, code, err)
			}
		})
	}
	check("claude")
	if _, err := os.Stat(retired); err != nil {
		t.Fatalf("a one-provider check pruned a root it cannot attribute: %v", err)
	}
	// A linked editor-session store cannot be listed, so its roots may still be live.
	sessions := filepath.Join(home, ".local", "state", "coop", "sessions", "acp")
	if err := os.MkdirAll(filepath.Dir(sessions), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), sessions); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := os.Stat(retired); err != nil {
		t.Fatalf("a check with an incomplete root listing pruned the cache: %v", err)
	}
	if err := os.Remove(sessions); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := os.Stat(retired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a full check kept a retired root: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(a.cfg.BoxHome, "usage-cache", "*", "*.json")); len(entries) != 1 {
		t.Fatalf("the live transcript's entry is missing: %v", entries)
	}
}

// A root listing that fails hides live history, so the report must say it is partial and the
// check must keep the hidden roots' cache entries rather than prune them as unused.
func TestUsageListingErrorIsPartialAndKeepsCache(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory regardless of its mode")
	}
	for _, tc := range []struct{ locked, failed string }{
		{"profiles", "every Claude account"},               // Config.Profiles
		{"credentials", "every Claude account"},            // EffectiveProfiles
		{"native-homes/work", "every history folder"},      // HistoryStores
		{"native-homes/work/repo", "every history folder"}, // one home of a listed account
		{"acp-homes", "every history folder"},              // ACPHistoryStores
	} {
		locked := tc.locked
		t.Run(strings.ReplaceAll(locked, "/", "-"), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GEMINI_CLI_HOME", "GROK_HOME"} {
				t.Setenv(key, "")
			}
			t.Setenv("NO_COLOR", "1")
			a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir(), RuntimeName: "must-not-run"}}
			claude := filepath.Join(a.cfg.ConfigDir, "claude")
			// One transcript in each kind of root: native, a profile, the repository homes of a
			// profile and of an account known only by its stored credentials, and an editor home.
			for i, root := range []string{
				filepath.Join(home, ".claude"),
				filepath.Join(claude, "profiles", "work"),
				filepath.Join(claude, "native-homes", "work", "repo", "home"),
				filepath.Join(claude, "native-homes", "solo", "repo", "home"),
				filepath.Join(claude, "acp-homes", "repo", "home"),
			} {
				data := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"r%d","message":{"id":"m%d","model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20}}}`, time.Now().UTC().Format(time.RFC3339Nano), i, i)
				usageFixtureFile(t, filepath.Join(root, "projects", "p", "s.jsonl"), data)
			}
			if err := os.MkdirAll(filepath.Join(claude, "credentials", "solo"), 0o700); err != nil {
				t.Fatal(err)
			}
			check := func() string {
				t.Helper()
				return captureStdout(t, func() {
					if code, err := a.cmdUsage(nil); code != 0 || err != nil {
						t.Fatalf("usage = (%d, %v)", code, err)
					}
				})
			}
			if out := check(); strings.Contains(out, "partial") {
				t.Fatalf("a complete check reported partial:\n%s", out)
			}
			entries, _ := filepath.Glob(filepath.Join(a.cfg.BoxHome, "usage-cache", "*", "*.json"))
			if len(entries) != 5 {
				t.Fatalf("cache entries = %v, want one per transcript", entries)
			}
			// Search but no read: the folder cannot be listed, yet paths through it still resolve, so
			// only the failed listing can make this check partial.
			dir := filepath.Join(claude, locked)
			if err := os.Chmod(dir, 0o100); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			// The warning names what failed, with the folder and the reason on the line beneath it.
			want := "  ⚠ Could not list " + tc.failed + ", so usage is partial\n    open " + dir
			if out := check(); !strings.Contains(out, want) || !strings.Contains(out, ": permission denied\n") {
				t.Fatalf("a check that could not list %s looked complete:\n%s", locked, out)
			}
			for _, entry := range entries {
				if _, err := os.Stat(entry); err != nil {
					t.Fatalf("a check that could not list %s pruned the cache: %v", locked, err)
				}
			}
		})
	}
}

// A failed listing belongs to its provider. A failed account listing replaces the sign-in line,
// since the accounts may be in what it could not list, and makes an exact account it may hide a
// listing error. A failed history folder hides no account: the sign-in line and a missing account
// stay. Either way the block names what failed and why, and the other providers stay whole.
func TestUsageListingErrorStaysWithItsProvider(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory regardless of its mode")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GEMINI_CLI_HOME", "GROK_HOME"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_COLOR", "1")
	data := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"native","message":{"id":"m","model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20}}}`, time.Now().UTC().Format(time.RFC3339Nano))
	usageFixtureFile(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"), data)
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir(), RuntimeName: "must-not-run"}}
	// Codex's only account and Gemini's editor homes are in folders that can be searched but not
	// listed.
	profiles := filepath.Dir(a.cfg.AgentProfileDir("codex", "work"))
	homes := filepath.Join(a.cfg.ConfigDir, "gemini", "acp-homes")
	for _, dir := range []string{a.cfg.AgentProfileDir("codex", "work"), homes} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{profiles, homes} {
		if err := os.Chmod(dir, 0o100); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}
	out := captureStdout(t, func() {
		if code, err := a.cmdUsage(nil); code != 0 || err != nil {
			t.Fatalf("usage = (%d, %v)", code, err)
		}
	})
	claude, rest, _ := strings.Cut(out, "\nCodex\n")
	codex, rest, _ := strings.Cut(rest, "\nGemini\n")
	gemini, grok, _ := strings.Cut(rest, "\nGrok\n")
	if want := "  ⚠ Could not list every Codex account, so usage is partial\n    open " + profiles + ": permission denied\n"; codex != want {
		t.Errorf("Codex block = %q, want only %q", codex, want)
	}
	if want := "  No credentials. Sign in: coop login gemini\n\n  ⚠ Could not list every history folder, so usage is partial\n    open " + homes + ": permission denied\n"; gemini != want {
		t.Errorf("Gemini block = %q, want %q", gemini, want)
	}
	if !strings.Contains(claude, "Subtotal  ≈$") || strings.Contains(claude, "partial") || strings.Contains(claude, "⚠") || strings.Contains(grok, "⚠") {
		t.Errorf("one provider's failed listing marked another partial:\n%s", out)
	}
	if code, err := a.cmdUsage([]string{"codex@work"}); code != 1 || err == nil || !strings.Contains(err.Error(), "Could not list every Codex account") {
		t.Errorf("usage codex@work = (%d, %v); want the listing error", code, err)
	}
	if code, err := a.cmdUsage([]string{"gemini@nobody"}); code != 2 || err == nil || !strings.Contains(err.Error(), "No Gemini account named") {
		t.Errorf("usage gemini@nobody = (%d, %v); want the missing account", code, err)
	}
}

// The warning headline is amber like every exception; its cause is an error cause, never dimmed.
func TestUsageListingWarningLeavesItsCauseUndimmed(t *testing.T) {
	pal := ui.Colored()
	var out bytes.Buffer
	rows := []usageCredential{{provider: "codex", account: "Unattributed ACP", shared: true}}
	cause := &os.PathError{Op: "open", Path: "/config/codex/profiles", Err: syscall.EACCES}
	renderUsage(&out, pal, 120, time.Now(), []string{"codex"}, rows, usageUnlisted{accounts: map[string]error{"codex": cause}}, false)
	want := pal.Yellow("  ⚠ Could not list every Codex account, so usage is partial") + "\n    open /config/codex/profiles: permission denied\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Fatalf("warning = %q, want it to end with %q", out.String(), want)
	}
}

func TestUsageNativeAndEditorDedupPreservesOwnership(t *testing.T) {
	e := agents.UsageEvent{ID: "request", Model: "gpt-6.1-sol", Time: time.Now(), Input: 100, Output: 10, WriteKnown: true, ContextKnown: true}
	makeRow := func(account string, shared, local bool) usageCredential {
		return usageCredential{provider: "codex", account: account, shared: shared, local: local, history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{e}}}
	}
	for _, tc := range []struct {
		name  string
		rows  []usageCredential
		owner int
	}{
		{"named", []usageCredential{makeRow("a", false, false), makeRow("editor", true, false), makeRow("native", true, true)}, 0},
		{"shared", []usageCredential{makeRow("editor", true, false), makeRow("native", true, true)}, 0},
		{"ambiguous", []usageCredential{makeRow("a", false, false), makeRow("b", false, false), makeRow("editor", true, false), makeRow("native", true, true)}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := deduplicateUsageCredentials(tc.rows)
			ag, _ := agents.Get("codex")
			priced := 0
			for i := range rows {
				rows[i].value = agents.ValueUsageHistory(rows[i].history, time.Now(), ag.Usage().Price)
				priced += rows[i].value.Priced
				if i != tc.owner && rows[i].value.Priced != 0 {
					t.Fatalf("duplicate owner %d", i)
				}
			}
			if priced != 1 {
				t.Fatalf("priced=%d", priced)
			}
		})
	}
}

func TestUsageNativeCopiesKeepBetterVersionAndDoNotInventSharedZero(t *testing.T) {
	e := agents.UsageEvent{ID: "r", Model: "gpt-6.1-sol", Time: time.Now(), Input: 100, Output: 10, WriteKnown: true, ContextKnown: true, Partial: true}
	final := e
	final.Partial = false
	final.Output = 20
	rows := deduplicateUsageCredentials([]usageCredential{
		{provider: "codex", account: "work", history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{e}}},
		{provider: "codex", account: "native", shared: true, local: true, history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{final}}},
		{provider: "claude", account: "native", shared: true, local: true, history: agents.UsageHistory{Available: true, Events: []agents.UsageEvent{final}}},
	})
	ag, _ := agents.Get("codex")
	value := agents.ValueUsageHistory(rows[0].history, time.Now(), ag.Usage().Price)
	if value.Priced != 1 || value.Partial || !rows[0].local || rows[1].history.Available || len(rows[2].history.Events) != 1 {
		t.Fatalf("%+v value=%+v", rows, value)
	}
}
