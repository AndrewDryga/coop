package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/ui"
)

// presetsRepo lays out a repo with one valid preset ("frontier") and one broken one.
func presetsRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	good := filepath.Join(repo, ".agent", "presets", "frontier")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "lead:\n" +
		"  agent: [claude:claude-fable-5@work, codex:gpt-5.6-sol/xhigh]\n" +
		"  prompt: roles/lead.md\n" +
		"roles:\n" +
		"  critic: {mode: consult, agent: [codex:gpt-5.6-sol/xhigh, grok:grok-4.5/high]}\n" +
		"  fast: {mode: delegate, agent: gemini:gemini-3.5-flash, when: [boilerplate, bulk-edits], prompt: roles/fast.md}\n"
	if err := os.WriteFile(filepath.Join(good, "preset.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	// The critic deliberately configures no prompt: its block must cost no Prompt line.
	if err := os.MkdirAll(filepath.Join(good, "roles"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"lead.md", "fast.md"} {
		if err := os.WriteFile(filepath.Join(good, "roles", f), []byte("extra\n"), 0o644); err != nil {
			t.Fatal(err)
		}
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

// The preset projection is ONE renderer with two doors: `coop presets <name>` and
// `coop help <name>` must print the same bytes, or the two will drift apart the first time
// somebody edits one of them.
func TestPresetDetailIsOneRendererBehindBothDoors(t *testing.T) {
	repo := presetsRepo(t)
	cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), BoxHome: t.TempDir()}
	a := &app{cfg: cfg}

	show := captureStdout(t, func() {
		if code, err := a.cmdPresets([]string{"frontier"}); code != 0 || err != nil {
			t.Errorf("cmdPresets(frontier) = (%d, %v)", code, err)
		}
	})
	help := captureStdout(t, func() {
		if code := helpForCommand("frontier", cfg); code != 0 {
			t.Errorf("helpForCommand(frontier) = %d, want 0", code)
		}
	})
	if show != help {
		t.Errorf("coop presets <name> and coop help <name> drifted:\n--- presets ---\n%s\n--- help ---\n%s", show, help)
	}
	want := `frontier — a preset for multiple models and providers to work together

Run it
  coop frontier
  coop loop frontier
  coop acp frontier

Lead models — next selected when the previous is unavailable:
  claude:claude-fable-5@work
  codex:gpt-5.6-sol/xhigh
  Prompt: .agent/presets/frontier/roles/lead.md

Roles available to the lead
  critic   Mode: consult — read-only advice
           Agent: codex:gpt-5.6-sol/xhigh, grok:grok-4.5/high

  fast     Mode: delegate — edits files; never commits; runs one at a time
           Agent: gemini:gemini-3.5-flash
           When: boilerplate and bulk edits
           Prompt: .agent/presets/frontier/roles/fast.md

Edit this preset
  .agent/presets/frontier/preset.yaml

For a guide to creating and using presets:
  coop help presets
`
	if show != want {
		t.Errorf("preset projection drifted:\n--- got ---\n%s\n--- want ---\n%s", show, want)
	}
}

// A role with no routing hints and no prompt costs no line — an absent prompt never prints
// "Coop default" or an empty label — and every label starts at ONE column, however long the
// longest role name is.
func TestPresetDetailBlocksAlignOnTheLongestRoleName(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".agent", "presets", "wide")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "lead: {agent: claude}\n" +
		"roles:\n" +
		"  documentation-reviewer: {mode: consult, agent: codex, when: [docs]}\n" +
		"  fast: {mode: delegate, agent: codex}\n"
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := preset.Load(repo, "", "wide")
	if err != nil {
		t.Fatal(err)
	}
	out := presetDetail(p, repo, ui.Palette{})
	column := -1
	for _, line := range strings.Split(out, "\n") {
		for _, label := range []string{"Mode:", "Agent:", "When:"} {
			at := strings.Index(line, label)
			if at < 0 {
				continue
			}
			if column == -1 {
				column = at
			}
			if at != column {
				t.Errorf("label %q starts at column %d, want %d (one gutter across the preset): %q", label, at, column, line)
			}
		}
	}
	if column != 2+len("documentation-reviewer")+3 {
		t.Errorf("gutter = %d, want the longest role name plus its separator", column)
	}
	if strings.Contains(out, "Prompt:") {
		t.Errorf("no role configures a prompt, so no Prompt row belongs here:\n%s", out)
	}
	if n := strings.Count(out, "When:"); n != 1 {
		t.Errorf("only one role has routing hints, got %d When rows:\n%s", n, out)
	}
	// One lead target cannot fall back, so it never promises it will.
	if strings.Contains(out, "Lead models") {
		t.Errorf("a single lead target should read as one model:\n%s", out)
	}
}

// `coop help <name>` resolves like a run does: a built-in command and a registered agent win a
// name collision, then a project preset, then a global one. A name nobody claims is unknown
// (exit 2), and a preset whose YAML is broken returns THAT error rather than pretending the
// name doesn't exist. All of it is file-only — no runtime, no box.
func TestHelpTopicResolutionOrder(t *testing.T) {
	repo := t.TempDir()
	global := t.TempDir()
	write := func(root, name, body string) {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	projectRoot := filepath.Join(repo, ".agent", "presets")
	// A project preset and a global one under the same name, plus presets named after a
	// command and an agent — neither may shadow the real page.
	write(projectRoot, "shared", "lead: {agent: claude:opus}\n")
	write(global, "shared", "lead: {agent: codex:gpt-5.6-sol}\n")
	write(global, "onlyglobal", "lead: {agent: grok:grok-4.5}\n")
	write(projectRoot, "loop", "lead: {agent: claude:opus}\n")
	write(projectRoot, "gemini", "lead: {agent: claude:opus}\n")
	write(projectRoot, "wrecked", "lead: {agent: claude}\nroles: {x: {mode: nonsense, agent: codex}}\n")

	cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), BoxHome: global}
	t.Setenv("COOP_PRESETS_DIR", global)
	run := func(topic string) (string, int) {
		t.Helper()
		var code int
		out := captureStdout(t, func() { code = helpForCommand(topic, cfg) })
		return out, code
	}

	if out, code := run("shared"); code != 0 || !strings.Contains(out, "claude:opus") {
		t.Errorf("project preset should win over the global one: (%d)\n%s", code, out)
	}
	if out, code := run("onlyglobal"); code != 0 || !strings.Contains(out, "grok:grok-4.5") {
		t.Errorf("a global preset is a help topic too: (%d)\n%s", code, out)
	}
	if out, code := run("loop"); code != 0 || !strings.Contains(out, "coop loop") || strings.Contains(out, "Run it") {
		t.Errorf("the loop COMMAND keeps its page against a preset of the same name: (%d)\n%s", code, out)
	}
	if out, code := run("gemini"); code != 0 || !strings.Contains(out, "run Gemini in a sandboxed box") {
		t.Errorf("the gemini AGENT keeps its page against a preset of the same name: (%d)\n%s", code, out)
	}
	if _, code := run("nonsense-name"); code != 2 {
		t.Errorf("unknown topic = %d, want 2", code)
	}
	broken := captureStderr(t, func() {
		if code := helpForCommand("wrecked", cfg); code != 2 {
			t.Errorf("a broken preset = %d, want 2", code)
		}
	})
	if !strings.Contains(broken, "mode \"nonsense\"") {
		t.Errorf("a broken preset should surface its validation error, not an unknown command:\n%s", broken)
	}
}

// A global preset's paths stay usable and its origin is labeled, so a file outside the checkout
// is never mistaken for one inside it.
func TestPresetDetailLabelsAGlobalOrigin(t *testing.T) {
	repo := t.TempDir()
	global := t.TempDir()
	dir := filepath.Join(global, "review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte("lead: {agent: claude:opus}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := preset.Load(repo, global, "review")
	if err != nil {
		t.Fatal(err)
	}
	out := presetDetail(p, repo, ui.Palette{})
	if !strings.Contains(out, filepath.ToSlash(dir)+"/preset.yaml (global)") {
		t.Errorf("a global preset shows its own path, labeled:\n%s", out)
	}
	if !strings.Contains(out, "review — a preset that runs Claude") {
		t.Errorf("the summary comes from the preset's shape, not its name:\n%s", out)
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
	// init also writes every prompt file the recipe references (all under roles/) — including
	// the critic's, so the show view has a real path to print beneath all four.
	for _, rel := range []string{"lead.md", "thinker.md", "critic.md", "fast.md"} {
		if _, err := os.Stat(filepath.Join(a.cfg.RepoOverride, ".agent", "presets", "frontier", "roles", rel)); err != nil {
			t.Errorf("init should scaffold roles/%s: %v", rel, err)
		}
	}
	show := captureStdout(t, func() {
		if code, err := a.cmdPresets([]string{"frontier"}); code != 0 || err != nil {
			t.Errorf("cmdPresets(frontier) = (%d, %v)", code, err)
		}
	})
	for _, rel := range []string{"lead.md", "thinker.md", "critic.md", "fast.md"} {
		if !strings.Contains(show, "Prompt: .agent/presets/frontier/roles/"+rel) {
			t.Errorf("show should point at the scaffolded roles/%s:\n%s", rel, show)
		}
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
