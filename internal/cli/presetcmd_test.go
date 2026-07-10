package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// presetsRepo lays out a repo with one valid preset ("frontier") and one broken one.
func presetsRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	good := filepath.Join(repo, ".agent", "presets", "frontier")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "lead: {agent: claude, models: [claude-fable-5@work]}\n" +
		"roles:\n" +
		"  critic: {mode: consult, agent: codex:gpt-5.5}\n" +
		"  fast: {mode: delegate, agent: gemini, when: [boilerplate]}\n"
	if err := os.WriteFile(filepath.Join(good, "preset.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(repo, ".agent", "presets", "broken")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "preset.yaml"), []byte("roles: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// `coop presets` is a real command: bare lists every preset (a broken one shows its
// error instead of hiding), `coop presets <name>` shows the full recipe, and the usual
// grammar rules hold (`ls` redirects, extra args error, unknown preset fails loud).
func TestCmdPresets(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: presetsRepo(t), ConfigDir: t.TempDir()}}

	list := captureStdout(t, func() {
		if code, err := a.cmdPresets(nil); code != 0 || err != nil {
			t.Errorf("cmdPresets() = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{"frontier", "lead claude/claude-fable-5", "critic (consult codex)", "fast (delegate gemini)", "broken", "lead.agent is required"} {
		if !strings.Contains(list, want) {
			t.Errorf("listing missing %q:\n%s", want, list)
		}
	}

	show := captureStdout(t, func() {
		if code, err := a.cmdPresets([]string{"frontier"}); code != 0 || err != nil {
			t.Errorf("cmdPresets(frontier) = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{"lead", "claude", "models claude-fable-5@work", "consult codex", "delegate gemini", "for: boilerplate", "coop loop --preset frontier"} {
		if !strings.Contains(show, want) {
			t.Errorf("show missing %q:\n%s", want, show)
		}
	}

	if code, err := a.cmdPresets([]string{"ghost"}); code != 2 || err == nil || !strings.Contains(err.Error(), "no preset") {
		t.Errorf("unknown preset = (%d, %v), want a loud miss", code, err)
	}
	if code, err := a.cmdPresets([]string{"ls"}); code != 2 || err == nil || !strings.Contains(err.Error(), "already lists") {
		t.Errorf("presets ls = (%d, %v), want the redirect", code, err)
	}
	if code, err := a.cmdPresets([]string{"a", "b"}); code != 2 || err == nil {
		t.Errorf("extra args = (%d, %v), want a usage error", code, err)
	}

	// An empty repo lists nothing, with the pointer to the scaffolder.
	b := &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}}
	empty := captureStdout(t, func() {
		if code, err := b.cmdPresets(nil); code != 0 || err != nil {
			t.Errorf("cmdPresets(empty) = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(empty, "no presets") || !strings.Contains(empty, "coop presets init") {
		t.Errorf("empty listing should point at the scaffolder:\n%s", empty)
	}
}

// `coop presets init [name]` scaffolds the documented template (default name frontier),
// which then lists and shows like any hand-written preset; re-init refuses to clobber.
func TestCmdPresetsInit(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}}
	if code, err := a.cmdPresets([]string{"init"}); code != 0 || err != nil {
		t.Fatalf("presets init = (%d, %v)", code, err)
	}
	list := captureStdout(t, func() {
		if code, err := a.cmdPresets(nil); code != 0 || err != nil {
			t.Errorf("cmdPresets() after init = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{"frontier", "lead claude/claude-fable-5", "thinker (native claude)", "critic (consult codex)", "fast (delegate gemini)"} {
		if !strings.Contains(list, want) {
			t.Errorf("scaffolded preset should list cleanly, missing %q:\n%s", want, list)
		}
	}
	// init also writes the prompt files the recipe references (all under roles/), so the
	// show view marks them.
	for _, rel := range []string{filepath.Join("roles", "lead.md"), filepath.Join("roles", "fast.md")} {
		if _, err := os.Stat(filepath.Join(a.cfg.RepoOverride, ".agent", "presets", "frontier", rel)); err != nil {
			t.Errorf("init should scaffold %s: %v", rel, err)
		}
	}
	show := captureStdout(t, func() {
		if code, err := a.cmdPresets([]string{"frontier"}); code != 0 || err != nil {
			t.Errorf("cmdPresets(frontier) = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(show, "+roles/lead.md") || !strings.Contains(show, "+md") {
		t.Errorf("show should mark the scaffolded prompt files (+roles/lead.md / +md):\n%s", show)
	}
	if code, err := a.cmdPresets([]string{"init"}); code != 2 || err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("re-init = (%d, %v), want a refusal", code, err)
	}
	// A custom name lands under its own folder; extra args and bad names are usage errors.
	if code, err := a.cmdPresets([]string{"init", "myteam"}); code != 0 || err != nil {
		t.Fatalf("presets init myteam = (%d, %v)", code, err)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.RepoOverride, ".agent", "presets", "myteam", "preset.yaml")); err != nil {
		t.Errorf("named init should write its own folder: %v", err)
	}
	if code, err := a.cmdPresets([]string{"init", "a", "b"}); code != 2 || err == nil {
		t.Errorf("extra init args = (%d, %v), want a usage error", code, err)
	}
	if code, err := a.cmdPresets([]string{"init", "../evil"}); code != 2 || err == nil {
		t.Errorf("bad init name = (%d, %v), want a refusal", code, err)
	}
}
