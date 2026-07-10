package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// modelsApp builds an app whose vault has one claude credential ("work"), so the
// credential path grammar has a real credential to act on.
func modelsApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "claude", "profiles", "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &app{cfg: &config.Config{ConfigDir: dir}}
}

// TestCredentialsModelRetired: v3 dropped model-on-credential — a credential is just an
// account, the model is a separate axis. Both the path form and the verb-first form
// tombstone (exit 2) pointing at --model / presets.
func TestCredentialsModelRetired(t *testing.T) {
	a := modelsApp(t)
	for _, args := range [][]string{
		{"claude", "work", "model", "opus"}, // path form
		{"claude", "work", "model"},         // path form, no value
		{"model", "claude", "work", "opus"}, // verb-first form
	} {
		code, err := a.cmdCredentials(args)
		if code != 2 || err == nil || !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "target") {
			t.Errorf("cmdCredentials(%v) = (%d, %v), want a retired-with-rewrite error pointing at the target", args, code, err)
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

// TestModelsIsAMenuNotAProfileDump: `coop models` shows the per-agent menu and how-to —
// not a row per credential (the dense wall the menu once drowned under).
func TestModelsIsAMenuNotAProfileDump(t *testing.T) {
	a := modelsApp(t)
	out := captureStdout(t, func() {
		if code, err := a.cmdModels(nil); code != 0 || err != nil {
			t.Errorf("cmdModels = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{"fable", "gpt-5", "gemini-2.5-pro", "one run", "preset"} {
		if !strings.Contains(out, want) {
			t.Errorf("menu missing %q:\n%s", want, out)
		}
	}
	// The old dump rendered one row per credential ("  work  … (mark: …)" + a "(default
	// profile)" tag). "\n  work" anchors the name as a row, not a substring — the caption's
	// "works" must not trip it.
	for _, reject := range []string{"\n  work", "mark:", "(default profile)"} {
		if strings.Contains(out, reject) {
			t.Errorf("menu still dumps per-credential rows (%q):\n%s", reject, out)
		}
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
