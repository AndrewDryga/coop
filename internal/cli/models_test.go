package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// modelsApp builds an app whose vault has one claude profile ("work"), so the
// profile-model subcommands have a real profile to mark.
func modelsApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "claude", "profiles", "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &app{cfg: &config.Config{ConfigDir: dir}}
}

// TestProfilesModelMarkAndClear: the mark round-trips through the path grammar —
// `coop profiles claude work model opus` sets it (visible to ModelFor on that profile),
// bare `model` reads it, `--clear` removes it. The verb form stays a working alias.
func TestProfilesModelMarkAndClear(t *testing.T) {
	a := modelsApp(t)
	if code, err := a.cmdProfiles([]string{"claude", "work", "model", "opus"}); code != 0 || err != nil {
		t.Fatalf("profiles <path> model = (%d, %v)", code, err)
	}
	if got := a.cfg.ProfileModelOf("claude", "work"); got != "opus" {
		t.Errorf("mark after set = %q, want opus", got)
	}
	a.cfg.SetActiveProfile("claude", "work")
	if got := a.cfg.ModelFor("claude"); got != "opus" {
		t.Errorf("ModelFor on the marked profile = %q, want opus", got)
	}
	// Bare `model` is the read (prints the mark; exercised for exit code here).
	if code, err := a.cmdProfiles([]string{"claude", "work", "model"}); code != 0 || err != nil {
		t.Errorf("profiles <path> model (read) = (%d, %v)", code, err)
	}
	if code, err := a.cmdProfiles([]string{"claude", "work", "model", "--clear"}); code != 0 || err != nil {
		t.Fatalf("profiles <path> model --clear = (%d, %v)", code, err)
	}
	if got := a.cfg.ProfileModelOf("claude", "work"); got != "" {
		t.Errorf("mark after clear = %q, want \"\"", got)
	}
	// Clearing an unmarked profile is a friendly no-op, not an error.
	if code, err := a.cmdProfiles([]string{"claude", "work", "model", "--clear"}); code != 0 || err != nil {
		t.Errorf("clearing an unmarked profile = (%d, %v), want a no-op", code, err)
	}
	// The verb-first form is retired (v3): it now tombstones with the path-grammar rewrite.
	if code, err := a.cmdProfiles([]string{"model", "claude", "work", "sonnet"}); code != 2 || err == nil {
		t.Errorf("verb-first profiles model should be retired (exit 2), got (%d, %v)", code, err)
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
	if code, err := a.cmdProfiles([]string{"claude", "work"}); code != 0 || err != nil {
		t.Errorf("profiles <agent> <profile> (show) = (%d, %v)", code, err)
	}
	if code, err := a.cmdProfiles([]string{"claude", "personal", "default"}); code != 0 || err != nil {
		t.Fatalf("profiles <path> default = (%d, %v)", code, err)
	}
	if got := a.cfg.DefaultProfileOf("claude"); got != "personal" {
		t.Errorf("default after path-form mark = %q, want personal", got)
	}
	for _, args := range [][]string{
		{"claude", "ghost"},                    // show: no such profile
		{"claude", "ghost", "model"},           // read: no such profile
		{"claude", "work", "colour", "opus"},   // unknown attribute
		{"claude", "work", "default", "extra"}, // stray token
		{"claude", "work", "model", "a", "b"},  // too many tokens
	} {
		if code, err := a.cmdProfiles(args); code != 2 || err == nil {
			t.Errorf("cmdProfiles(%v) = (%d, %v), want (2, error)", args, code, err)
		}
	}
}

func TestProfilesModelRejectsBadInput(t *testing.T) {
	a := modelsApp(t)
	cases := [][]string{
		{"borg", "work", "model", "opus"},        // unknown agent (falls to the verb path → unknown)
		{"claude", "ghost", "model", "opus"},     // no such profile
		{"claude", "work", "model", "--model"},   // a flag isn't a model
		{"claude", "work", "model", "two words"}, // whitespace would corrupt the models file
	}
	for _, args := range cases {
		if code, err := a.cmdProfiles(args); code != 2 || err == nil {
			t.Errorf("cmdProfiles(%v) = (%d, %v), want (2, error)", args, code, err)
		}
	}
	if got := readFileString(a.cfg.ModelsFile()); got != "" {
		t.Errorf("a rejected input wrote to the models file: %q", got)
	}
	// The models command itself is read-only: an unknown agent (or a mutation verb)
	// errors, and a stray token after a valid agent errors too.
	for _, args := range [][]string{{"borg"}, {"default"}, {"clear"}, {"claude", "extra"}} {
		if code, err := a.cmdModels(args); code != 2 || err == nil {
			t.Errorf("cmdModels(%v) = (%d, %v), want (2, error)", args, code, err)
		}
	}
}

// TestModelsIsAMenuNotAProfileDump: `coop models` shows the per-agent menu and how-to —
// per-profile rows (and their repeated "mark:" hint) moved to `coop profiles`, which was
// the dense wall the menu drowned under.
func TestModelsIsAMenuNotAProfileDump(t *testing.T) {
	a := modelsApp(t)
	if err := a.cfg.SetProfileModel("claude", "work", "opus"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if code, err := a.cmdModels(nil); code != 0 || err != nil {
			t.Errorf("cmdModels = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{"fable", "gpt-5", "gemini-2.5-pro", "per profile", "--model"} {
		if !strings.Contains(out, want) {
			t.Errorf("menu missing %q:\n%s", want, out)
		}
	}
	// The old dump rendered one row per profile ("  work  … (mark: …)" + a "(default
	// profile)" tag). "\n  work" anchors the profile name as a row, not a substring —
	// the caption's "works" must not trip it.
	for _, reject := range []string{"\n  work", "mark:", "(default profile)"} {
		if strings.Contains(out, reject) {
			t.Errorf("menu still dumps per-profile rows (%q):\n%s", reject, out)
		}
	}
}

// TestProfilesListsModelColumn: the profiles listing carries each profile's marked model
// (a column, with — for unmarked) — that's where per-profile model state lives.
func TestProfilesListsModelColumn(t *testing.T) {
	a := modelsApp(t)
	if err := a.cfg.SetProfileModel("claude", "work", "opus"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if code, err := a.cmdProfiles([]string{"claude"}); code != 0 || err != nil {
			t.Errorf("cmdProfiles = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(out, "opus") {
		t.Errorf("profiles listing missing the model column:\n%s", out)
	}
}

// TestModelsMarkClearedOnProfileRemoval: `coop profiles rm` drops the removed profile's
// model mark with it, so a stale mark can't linger in `coop models`.
func TestModelsMarkClearedOnProfileRemoval(t *testing.T) {
	a := modelsApp(t)
	// A second profile so "work" isn't the default (rm refuses the marked default).
	if err := os.MkdirAll(filepath.Join(a.cfg.ConfigDir, "claude", "profiles", "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.cfg.SetProfileModel("claude", "work", "opus"); err != nil {
		t.Fatal(err)
	}
	if code, err := a.cmdProfiles([]string{"claude", "work", "rm", "--yes"}); code != 0 || err != nil {
		t.Fatalf("profiles rm = (%d, %v)", code, err)
	}
	if got := a.cfg.ProfileModelOf("claude", "work"); got != "" {
		t.Errorf("mark survived the profile's removal: %q", got)
	}
}

func TestExtractRunModel(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantModel string
		wantRest  []string
		wantErr   bool
	}{
		{"none", []string{"-p", "hi"}, "", []string{"-p", "hi"}, false},
		{"space form", []string{"--model", "opus", "-p", "hi"}, "opus", []string{"-p", "hi"}, false},
		{"equals form", []string{"--model=opus"}, "opus", nil, false},
		{"missing value", []string{"--model"}, "", nil, true},
		// coop reads --model only before --; the agent's own --model passes through verbatim.
		{"passthrough after --", []string{"--", "--model", "raw"}, "", []string{"--", "--model", "raw"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model, rest, err := extractRunModel(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if model != c.wantModel || !slices.Equal(rest, c.wantRest) {
				t.Errorf("extractRunModel(%v) = (%q, %v), want (%q, %v)", c.args, model, rest, c.wantModel, c.wantRest)
			}
		})
	}
}
