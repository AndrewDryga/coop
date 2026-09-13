package cli

import (
	"bufio"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/scaffold"
)

// The first-run menus offer the exact tokens they accept, and refuse to guess at anything else:
// an unknown answer is named and the question comes back, so half an answer is never silently
// applied as if it were the whole one.
func TestPromptExactTokens(t *testing.T) {
	ask := func(input string) (string, []string, []string) {
		t.Helper()
		sc := bufio.NewScanner(strings.NewReader(input))
		var langs, services []string
		out := captureStderr(t, func() {
			langs = promptGateLangs(sc)
			services = promptServices(sc)
		})
		return out, langs, services
	}
	// One prompt's menu holds only the accepted tokens — never the formatter a language implies,
	// which would read as a fifth choice the parser rejects.
	out, langs, services := ask("terraform go go\npostgres\n")
	if !slices.Equal(langs, []string{"terraform", "go"}) || !slices.Equal(services, []string{"postgres"}) {
		t.Errorf("answers = %v / %v, want [terraform go] / [postgres]", langs, services)
	}
	for _, absent := range []string{"gofmt", "terraform fmt", "mix format", "rustfmt"} {
		if strings.Contains(out, absent) {
			t.Errorf("the menu offered %q, which is not an accepted answer:\n%s", absent, out)
		}
	}
	for _, want := range []string{"  go\n", "  terraform\n", "  elixir\n", "  rust\n", "  postgres\n", "  redis\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the menu is missing the accepted token %q:\n%s", want, out)
		}
	}
	// An unknown token is named and the SAME question repeats; the retry's answer is what counts.
	out, langs, _ = ask("rustfmt\ngo rust\n\n")
	if !slices.Equal(langs, []string{"go", "rust"}) {
		t.Errorf("re-prompt answer = %v, want [go rust]", langs)
	}
	if !strings.Contains(out, "Unknown language “rustfmt”") || !strings.Contains(out, "Choose from: go, terraform, elixir, rust") {
		t.Errorf("an unknown token was not named with its choices:\n%s", out)
	}
	if strings.Count(out, "Languages") < 2 {
		t.Errorf("the question did not repeat after an unknown token:\n%s", out)
	}
	// Blank means none. So does a closed stdin — a prompt with nothing left to read stops asking
	// instead of spinning on its own error.
	for _, in := range []string{"\n\n", ""} {
		if _, langs, services := ask(in); langs != nil || services != nil {
			t.Errorf("%q → %v / %v, want no selection", in, langs, services)
		}
	}
}

// Git is created only on an explicit yes, before the hooks that need it. A decline or a closed
// stdin creates nothing at all — coop never invents consent to touch a directory this way.
func TestAskGitInit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "none"))
	for _, c := range []struct {
		answer string
		want   bool
	}{{"y\n", true}, {"yes\n", true}, {"\n", true}, {"n\n", false}, {"no\n", false}, {"", false}} {
		repo := t.TempDir()
		out := captureStderr(t, func() {
			if err := askGitInit(bufio.NewScanner(strings.NewReader(c.answer)), repo); err != nil {
				t.Fatal(err)
			}
		})
		if got := pathExists(filepath.Join(repo, ".git")); got != c.want {
			t.Errorf("answer %q → .git exists = %v, want %v", c.answer, got, c.want)
		}
		for _, want := range []string{"This folder is not a Git repository.", "Coop uses Git for tasks, forks, and commit checks.", "Initialize Git here? [Y/n]"} {
			if !strings.Contains(out, want) {
				t.Errorf("the Git question is missing %q:\n%s", want, out)
			}
		}
	}
}

// A run with no terminal asks nothing, creates no .git behind the user's back, and still says
// exactly what would finish setup. The result is one outcome, not the file-by-file ledger.
func TestInitWithoutTerminalAsksNothingAndNamesTheMissingStep(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir, cfgDir := t.TempDir(), t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: dir, ConfigDir: cfgDir, MCPFile: filepath.Join(cfgDir, "mcp.json")}}
	out := captureStderr(t, func() {
		if code, err := a.cmdInit([]string{"--services", "none", "--agents", "claude,codex"}); code != 0 || err != nil {
			t.Fatalf("cmdInit = (%d, %v)", code, err)
		}
	})
	if pathExists(filepath.Join(dir, ".git")) {
		t.Error("a non-interactive init created a Git repository nobody asked for")
	}
	for _, want := range []string{
		"Setting up Coop",
		"✓ Coop project created",
		"Claude and Codex share instructions, skills, and one task queue.",
		"Network access is filtered.",
		"Claude can reach Anthropic and Codex can reach OpenAI.",
		"Other network traffic is blocked until you approve rules allowing it.",
		"Finish setup",
		"→ git init",
		"→ coop init",
		"Verify the sandbox\n  → coop doctor",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init result is missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{
		"Initialize Git here?", "Languages", "Services (",
		"wrote AGENTS.md", "linked CLAUDE.md", "added skill", "updated .gitignore", "commit gate:",
		"coop net setup", "kept ",
		dir, // the working directory is not echoed as a header; only actionable paths appear
	} {
		if strings.Contains(out, absent) {
			t.Errorf("init result should not contain %q:\n%s", absent, out)
		}
	}
	// A re-init keeps every file and repeats none of the first-run explanation.
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = captureStderr(t, func() {
		if code, err := a.cmdInit(nil); code != 0 || err != nil {
			t.Fatalf("re-init = (%d, %v)", code, err)
		}
	})
	if data, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")); err != nil || string(data) != "mine\n" {
		t.Errorf("re-init clobbered AGENTS.md: %q, %v", data, err)
	}
	if !strings.Contains(out, "✓ Coop project updated") {
		t.Errorf("re-init did not report the update:\n%s", out)
	}
	for _, absent := range []string{"Setting up Coop for", "Internet access is filtered.", "Start working", "Coop project created"} {
		if strings.Contains(out, absent) {
			t.Errorf("re-init repeated first-run output %q:\n%s", absent, out)
		}
	}
}

type initTreeEntry struct {
	info os.FileInfo
	data string
	link string
}

func snapshotInitTree(t *testing.T, root string) map[string]initTreeEntry {
	t.Helper()
	entries := map[string]initTreeEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := initTreeEntry{info: info}
		switch {
		case info.Mode().IsRegular():
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			entry.data = string(data)
		case info.Mode()&os.ModeSymlink != 0:
			entry.link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestInitStackPreflightPreservesTree(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "none"))
	for _, api := range []string{"scaffold", "cli"} {
		for _, state := range []string{"fresh", "initialized"} {
			for _, failure := range []string{"unknown", "unknown-with-tools", "asdf-missing-tools"} {
				t.Run(api+"/"+state+"/"+failure, func(t *testing.T) {
					root := t.TempDir()
					repo, cfgDir := filepath.Join(root, "repo"), filepath.Join(root, "config")
					write := func(rel, data string, mode os.FileMode) {
						t.Helper()
						path := filepath.Join(root, rel)
						if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte(data), mode); err != nil {
							t.Fatal(err)
						}
					}
					write("repo/README.md", "project\n", 0o644)
					write("config/keep.conf", "preserve config\n", 0o600)
					write("outside/canary", "outside data\n", 0o600)
					if err := os.Symlink("../outside", filepath.Join(repo, "outside-link")); err != nil {
						t.Fatal(err)
					}
					if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
						t.Fatalf("git init: %v, %s", err, out)
					}
					if state == "initialized" {
						if _, err := scaffold.Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
							t.Fatal(err)
						}
						write("repo/AGENTS.md", "custom instructions\n", 0o600)
						write("repo/.githooks/pre-commit", "#!/bin/sh\nexit 0\n", 0o750)
						write("repo/.agent/Dockerfile", "FROM custom\n", 0o640)
					}
					if failure == "unknown-with-tools" {
						write("repo/.tool-versions", "golang 1.26.4\n", 0o644)
					}
					stack := "not-a-stack"
					if failure == "asdf-missing-tools" {
						stack = "asdf"
					}
					a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: cfgDir, MCPFile: filepath.Join(cfgDir, "mcp.json")}}
					run := func() error {
						if api == "scaffold" {
							_, err := scaffold.Init(repo, stack, []string{"go"}, []string{"claude", "codex", "gemini"})
							return err
						}
						_, err := a.cmdInit([]string{"--stack", stack, "--services", "postgres", "--agents", "all"})
						return err
					}
					before := snapshotInitTree(t, root)
					// A file keeps the old implementation's long progress output from
					// filling a pipe while this regression is expected to fail.
					log, err := os.CreateTemp(t.TempDir(), "init-output-")
					if err != nil {
						t.Fatal(err)
					}
					old := os.Stderr
					os.Stderr = log
					defer func() { os.Stderr = old; _ = log.Close() }()
					runErr := run()
					os.Stderr = old
					if err := log.Close(); err != nil {
						t.Fatal(err)
					}
					if runErr == nil {
						t.Fatal("invalid stack was not refused")
					}
					// The refusal itself is the only thing a denied init may print: it names the
					// flag and what to do, and nothing before it claims scaffold work happened.
					data, err := os.ReadFile(log.Name())
					if err != nil {
						t.Fatal(err)
					}
					printed := string(data)
					if api == "scaffold" {
						if len(printed) != 0 {
							t.Errorf("the scaffold package printed %d bytes; the caller owns the refusal: %q", len(printed), printed)
						}
					} else if !strings.Contains(printed, "--stack") {
						t.Errorf("the refusal did not name the flag it refused: %q", printed)
					}
					for _, leaked := range []string{"Coop project created", "Coop project updated", "Setting up Coop"} {
						if strings.Contains(printed, leaked) {
							t.Errorf("a denied init claimed setup work (%q): %q", leaked, printed)
						}
					}
					after := snapshotInitTree(t, root)
					if len(before) != len(after) {
						t.Fatalf("denied init changed tree inventory: %d -> %d entries", len(before), len(after))
					}
					for name, want := range before {
						got, ok := after[name]
						if !ok || !os.SameFile(want.info, got.info) || want.info.Mode() != got.info.Mode() || want.data != got.data || want.link != got.link {
							t.Fatalf("denied init changed %s", name)
						}
					}
					if failure == "asdf-missing-tools" {
						write("repo/.tool-versions", "golang 1.26.4\n", 0o644)
					} else {
						stack = ""
					}
					if err := run(); err != nil {
						t.Fatalf("corrected retry failed: %v", err)
					}
					if !scaffold.Initialized(repo) {
						t.Fatal("corrected retry did not initialize the repo")
					}
					if api == "cli" {
						for _, path := range []string{a.cfg.MCPFile, filepath.Join(repo, ".agent", "compose.yml")} {
							if _, err := os.Stat(path); err != nil {
								t.Fatalf("valid retry did not write %s: %v", path, err)
							}
						}
					}
				})
			}
		}
	}
}

// A fresh init selects filtered access explicitly and, asking for nothing a
// human must approve, prints no network notice at all; once the file asks for a
// website, a re-init adds exactly the two lines that say so — with no Docker
// and no host setup anywhere in the picture.
func TestInitReportsAPendingNetworkRequestOnlyWhenThereIsOne(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "none"))
	repo, cfgDir := t.TempDir(), t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v, %s", err, out)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: cfgDir, MCPFile: filepath.Join(cfgDir, "mcp.json")}}
	initOutput := func() string {
		t.Helper()
		return captureStderr(t, func() {
			if code, err := a.cmdInit([]string{"--services", "none", "--agents", "claude"}); code != 0 || err != nil {
				t.Errorf("cmdInit = (%d, %v)", code, err)
			}
		})
	}
	out := initOutput()
	// A fresh project has nothing pending, so init says nothing about approval.
	for _, absent := range []string{"coop approve", netPendingHeadline} {
		if strings.Contains(out, absent) {
			t.Errorf("a fresh init claimed a pending network request (%q):\n%s", absent, out)
		}
	}
	project := filepath.Join(repo, ".agent", "project.yaml")
	data, err := os.ReadFile(project)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\n  egress: filtered\n") {
		t.Errorf("a fresh project does not select filtered access:\n%s", data)
	}
	if err := os.WriteFile(project, []byte("box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: docs.example.com}\n      protocol: tls\n      ports: [443]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := "⚠ " + netPendingHeadline + "\n  .agent/project.yaml requests changes to network access.\n  " + netPendingReview + "\n"
	if out := initOutput(); !strings.HasSuffix(out, want) {
		t.Errorf("a pending request did not end init with the approval notice:\n%s\nwant to end with:\n%s", out, want)
	}
	// The file a human edited is not rewritten by the re-init.
	if after, _ := os.ReadFile(project); !strings.Contains(string(after), "docs.example.com") {
		t.Errorf("re-init rewrote project.yaml:\n%s", after)
	}
}
