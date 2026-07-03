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
	yaml := "lead: {agent: claude, model: claude-fable-5, credentials: [work]}\n" +
		"roles:\n" +
		"  critic: {mode: consult, agent: codex, model: gpt-5.5}\n" +
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
	for _, want := range []string{"lead", "claude", "credentials work", "consult codex", "delegate gemini", "for: boilerplate", "coop loop --preset frontier"} {
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

	// An empty repo lists nothing, with the pointer to the format.
	b := &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}}
	empty := captureStdout(t, func() {
		if code, err := b.cmdPresets(nil); code != 0 || err != nil {
			t.Errorf("cmdPresets(empty) = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(empty, "no presets") || !strings.Contains(empty, "coop help presets") {
		t.Errorf("empty listing should point at the format:\n%s", empty)
	}
}
