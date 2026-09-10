package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/config"
)

// modelsApp builds an app whose vault has one claude credential ("work"), so the
// credential path grammar has a real credential to act on.
//
// No unit test may reach a real provider: `coop models` now refreshes a due catalog by itself, so
// the app comes with an empty PATH (no agent CLI to probe) and a failing ACP fetcher stub (so no
// container runtime is ever detected, let alone a box spawned). A test that wants a fetch to
// succeed replaces a.acpModels; one that wants no fetch at all writes a fresh cache first.
func modelsApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "claude", "profiles", "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(dir, "no-agent-cli"))
	return &app{
		cfg:       &config.Config{ConfigDir: dir},
		acpModels: func(string) ([]acpctl.Model, error) { return nil, errors.New("no provider in tests") },
	}
}

// TestCredentialsModelIsNotAnAttribute: v3 dropped model-on-credential — a credential is just an
// account, the model is a separate axis. A `model` token (path form or leading verb) is now just an
// unknown attribute/agent (exit 2), not a special note.
func TestCredentialsModelIsNotAnAttribute(t *testing.T) {
	a := modelsApp(t)
	for _, args := range [][]string{
		{"claude", "work", "model", "opus"}, // path form
		{"claude", "work", "model"},         // path form, no value
		{"model", "claude", "work", "opus"}, // leading verb
	} {
		if code, err := a.cmdCredentials(args); code != 2 || err == nil {
			t.Errorf("cmdCredentials(%v) = (%d, %v), want (2, error)", args, code, err)
		}
	}
}

// TestProfilePathGrammar: the narrowing path — a profile shows its detail, `default`
// marks it, an unknown attribute or profile errors (exit 2).
func TestProfilePathGrammar(t *testing.T) {
	a := modelsApp(t)
	// A second profile so default-marking has something to move off of.
	if err := os.MkdirAll(filepath.Join(a.cfg.ConfigDir, "claude", "profiles", "personal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, err := a.cmdCredentials([]string{"claude", "work"}); code != 0 || err != nil {
		t.Errorf("profiles <agent> <profile> (show) = (%d, %v)", code, err)
	}
	if code, err := a.cmdCredentials([]string{"claude", "personal", "default"}); code != 0 || err != nil {
		t.Fatalf("profiles <path> default = (%d, %v)", code, err)
	}
	if got := a.cfg.DefaultProfileOf("claude"); got != "personal" {
		t.Errorf("default after path-form mark = %q, want personal", got)
	}
	for _, args := range [][]string{
		{"claude", "ghost"},                    // show: no such profile
		{"claude", "work", "colour", "opus"},   // unknown attribute
		{"claude", "work", "default", "extra"}, // stray token
	} {
		if code, err := a.cmdCredentials(args); code != 2 || err == nil {
			t.Errorf("cmdCredentials(%v) = (%d, %v), want (2, error)", args, code, err)
		}
	}
}

// The models command is read-only: an unknown agent (or a mutation verb like the retired
// `default`/`clear`) errors, and a stray token after a valid agent errors too.
func TestModelsCommandIsReadOnly(t *testing.T) {
	a := modelsApp(t)
	for _, args := range [][]string{{"borg"}, {"default"}, {"clear"}, {"claude", "extra"}} {
		if code, err := a.cmdModels(args); code != 2 || err == nil {
			t.Errorf("cmdModels(%v) = (%d, %v), want (2, error)", args, code, err)
		}
	}
}

// TestModelsIsACompactMenu: `coop models` is a block of ids per agent plus two copyable footers —
// no title, no repeated field labels, no healthy cache age, no credential rows, and none of the
// old one-run/standing/loop-steps/everywhere table.
func TestModelsIsACompactMenu(t *testing.T) {
	a := modelsApp(t)
	out := captureStdout(t, func() {
		if code, err := a.cmdModels(nil); code != 0 || err != nil {
			t.Errorf("cmdModels = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{
		"Claude\n", "fable", "gpt-5", "gemini-2.5-pro",
		"Start Claude with a model\n  → coop claude:fable\n",
		"Set models for presets and loops\n  → coop help models\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("menu missing %q:\n%s", want, out)
		}
	}
	// The old shapes: a title, per-block labels, a healthy age, the env var, the how-to table, and
	// the per-credential dump ("\n  work" anchors the name as a row, not a substring).
	for _, reject := range []string{
		"Models:", "Last refreshed", "COOP_CLAUDE_MODEL", "one run", "loop steps", "everywhere",
		"\n  work", "mark:", "(default profile)", "--refresh",
	} {
		if strings.Contains(out, reject) {
			t.Errorf("menu still shows %q:\n%s", reject, out)
		}
	}
}

// TestModelsShowsStandingDefaultAsAFact: a configured COOP_<AGENT>_MODEL is a human sentence in
// that agent's block — never a synthetic id in the list, never the env var's name.
func TestModelsShowsStandingDefaultAsAFact(t *testing.T) {
	a := modelsApp(t)
	t.Setenv("COOP_CLAUDE_MODEL", "fable/high") // the effort rides the same var; the fact is the model
	out := captureStdout(t, func() {
		if code, err := a.cmdModels([]string{"claude"}); code != 0 || err != nil {
			t.Errorf("cmdModels = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(out, "\n  Default for Claude runs: fable\n") {
		t.Errorf("menu missing the standing-default fact:\n%s", out)
	}
	if strings.Contains(out, "COOP_") {
		t.Errorf("the normal menu must not name the env var:\n%s", out)
	}
}

// TestWrapModelIDs: ids wrap to the terminal width without ever being split, and the width is
// measured on plain text — the caller styles the separator afterwards (no-color-in-width-fields).
func TestWrapModelIDs(t *testing.T) {
	ids := []string{"opus[1m]", "claude-fable-5", "sonnet", "haiku"}
	rows := wrapModelIDs(ids, 30) // "opus[1m] · claude-fable-5" is 25; adding " · sonnet" is 34
	want := [][]string{{"opus[1m]", "claude-fable-5"}, {"sonnet", "haiku"}}
	if len(rows) != len(want) {
		t.Fatalf("wrapModelIDs(30) = %v, want %v", rows, want)
	}
	for i := range want {
		if strings.Join(rows[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("row %d = %v, want %v", i, rows[i], want[i])
		}
	}
	// An id wider than the whole terminal still comes out whole — a model id you cannot copy is
	// worse than a long row.
	if rows := wrapModelIDs(ids, 4); len(rows) != 4 || rows[1][0] != "claude-fable-5" {
		t.Errorf("wrapModelIDs(4) = %v, want one whole id per row", rows)
	}
}

// The menu names the current ids the frontier preset recipe uses, so a preset author
// can copy them straight from `coop models`.
func TestModelsMenuHasFrontierIDs(t *testing.T) {
	out := captureStdout(t, func() {
		if code, err := modelsApp(t).cmdModels(nil); code != 0 || err != nil {
			t.Errorf("cmdModels = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{"claude-fable-5", "claude-opus-4-8", "gpt-5.5", "gemini-3.5-flash"} {
		if !strings.Contains(out, want) {
			t.Errorf("menu missing the recipe id %q:\n%s", want, out)
		}
	}
}
