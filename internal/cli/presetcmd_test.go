package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

// presetsRepo lays out a repo with one valid preset ("frontier") and one broken one.
func presetsRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	good := filepath.Join(repo, ".agent", "presets", "frontier")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "lead: {agent: claude:claude-fable-5@work}\n" +
		"roles:\n" +
		"  critic: {mode: consult, agent: [codex:gpt-5.6-sol/xhigh, grok:grok-4.5/high]}\n" +
		"  fast: {mode: delegate, agent: gemini:gemini-3.5-flash, when: [boilerplate]}\n"
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
	for _, want := range []string{"frontier", "lead claude:claude-fable-5", "critic (consult codex)", "fast (delegate gemini)", "broken", "lead.agent: is required"} {
		if !strings.Contains(list, want) {
			t.Errorf("listing missing %q:\n%s", want, list)
		}
	}

	show := captureStdout(t, func() {
		if code, err := a.cmdPresets([]string{"frontier"}); code != 0 || err != nil {
			t.Errorf("cmdPresets(frontier) = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{"lead", "claude", "ladder claude:claude-fable-5@work", "consult", "ladder codex:gpt-5.6-sol/xhigh, grok:grok-4.5/high", "delegate gemini", "model gemini-3.5-flash", "for: boilerplate", "coop loop frontier"} {
		if !strings.Contains(show, want) {
			t.Errorf("show missing %q:\n%s", want, show)
		}
	}
	if strings.Contains(show, "gemini-3.5-flash/") {
		t.Errorf("effort-free role has a dangling slash:\n%s", show)
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

func TestRoleTuning(t *testing.T) {
	for _, tc := range []struct {
		name                string
		role                preset.Role
		wantKind, wantValue string
	}{
		{"model and effort", preset.Role{Targets: []agents.Target{{Model: "gpt-5.6-sol", Effort: "xhigh"}}}, "model", "gpt-5.6-sol/xhigh"},
		{"model only", preset.Role{Targets: []agents.Target{{Model: "gemini-3.5-flash"}}}, "model", "gemini-3.5-flash"},
		{"effort only", preset.Role{Targets: []agents.Target{{Effort: "high"}}}, "effort", "high"},
		{"neither", preset.Role{}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, value := roleTuning(tc.role)
			if kind != tc.wantKind || value != tc.wantValue {
				t.Errorf("roleTuning(%+v) = (%q, %q), want (%q, %q)",
					tc.role, kind, value, tc.wantKind, tc.wantValue)
			}
		})
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
	for _, want := range []string{"frontier", "lead claude:claude-fable-5", "thinker (native claude)", "critic (consult codex)", "fast (delegate gemini)"} {
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

func TestCmdPresetsInitPreservesIncompleteDestinations(t *testing.T) {
	for _, shape := range []string{"empty", "prompt", "file", "dangling", "external"} {
		t.Run(shape, func(t *testing.T) {
			repo, outside := t.TempDir(), t.TempDir()
			dest := filepath.Join(repo, preset.Dir, "custom")
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				t.Fatal(err)
			}
			kept := dest
			switch shape {
			case "empty":
				if err := os.Mkdir(dest, 0o700); err != nil {
					t.Fatal(err)
				}
			case "prompt", "file":
				if shape == "prompt" {
					kept = filepath.Join(dest, "roles", "lead.md")
					if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(kept, []byte("my instructions"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "dangling", "external":
				target := outside
				if shape == "dangling" {
					target = filepath.Join(outside, "absent")
				}
				if err := os.Symlink(target, dest); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(kept)
			if err != nil {
				t.Fatal(err)
			}
			a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
			out := captureStdout(t, func() {
				code, err := a.cmdPresets([]string{"init", "custom"})
				if code != 2 || err == nil || !strings.Contains(err.Error(), "existing files were preserved") || !strings.Contains(err.Error(), "choose another name") {
					t.Fatalf("incomplete preset refusal = %d, %v", code, err)
				}
			})
			if out != "" {
				t.Fatalf("failed init announced success: %q", out)
			}
			after, err := os.Lstat(kept)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("CLI replaced existing entry: %v", err)
			}
			if shape == "prompt" || shape == "file" {
				if data, err := os.ReadFile(kept); err != nil || string(data) != "my instructions" {
					t.Fatal("CLI changed user content")
				}
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Fatalf("CLI wrote outside the repository: %v, %v", entries, err)
			}
		})
	}
}
