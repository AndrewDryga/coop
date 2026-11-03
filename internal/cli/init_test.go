package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/runtime"
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

// A run with no terminal asks nothing. Outside Git it refuses before it writes a file and creates
// no .git behind the user's back, naming the step that fixes it; in a repository the result is one
// outcome, not the file-by-file ledger.
func TestInitWithoutTerminalAsksNothingAndNamesTheMissingStep(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	hermeticGit(t)
	dir, cfgDir := t.TempDir(), t.TempDir()
	a := initAppOnDocker(dir, cfgDir)
	args := []string{"--services", "none", "--agents", "claude,codex"}
	if code, err := a.cmdInit(args); code != 1 || err == nil ||
		!strings.Contains(err.Error(), "This folder is not a Git repository") || !strings.Contains(err.Error(), "Run git init, then coop init again.") {
		t.Fatalf("cmdInit outside Git = (%d, %v), want the not-a-repository refusal", code, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("a refused init wrote into the folder, a .git included: %v, %v", entries, err)
	}
	gitInit(t, dir)
	out := captureStderr(t, func() {
		if code, err := a.cmdInit(args); code != 0 || err != nil {
			t.Fatalf("cmdInit = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{
		"Setting up Coop",
		"✓ Coop project created",
		"Claude and Codex share instructions, skills, and one task queue.",
		"Network access is filtered.",
		"Claude can reach Anthropic and Codex can reach OpenAI.",
		"Other network traffic is blocked until you approve rules allowing it.",
		"Verify the sandbox\n  → coop doctor",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init result is missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{
		"Initialize Git here?", "Languages", "Services (", "Finish setup",
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

// cannotFilterWarning is the block init prints when its runtime cannot filter network access.
var cannotFilterWarning = []string{"Apple container can't filter network access",
	"This project filters network access, which needs Docker.",
	"Start or install Docker. Online agents need Docker even with open networking."}

// On Apple container a filtered project is refused at its first run (checkFilteredRuntime), so
// init says so up front and directs the user to Docker. The committed policy stays filtered: a
// teammate on Docker keeps it. On Docker, init says nothing extra.
func TestInitWarnsWhenTheRuntimeCannotFilter(t *testing.T) {
	for _, tc := range []struct {
		runtime string
		warns   bool
	}{{"container", true}, {"docker", false}} {
		t.Run(tc.runtime, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			hermeticGit(t)
			dir, cfgDir := t.TempDir(), t.TempDir()
			gitInit(t, dir)
			a := &app{cfg: &config.Config{RepoOverride: dir, ConfigDir: cfgDir, MCPFile: filepath.Join(cfgDir, "mcp.json")},
				rt: runtime.Runtime{Name: tc.runtime}, rtSet: true}
			out := captureStderr(t, func() {
				if code, err := a.cmdInit([]string{"--services", "none", "--agents", "claude"}); code != 0 || err != nil {
					t.Fatalf("cmdInit = (%d, %v)", code, err)
				}
			})
			for _, line := range cannotFilterWarning {
				if strings.Contains(out, line) != tc.warns {
					t.Errorf("on %s, init output has %q = %t, want %t:\n%s", tc.runtime, line, !tc.warns, tc.warns, out)
				}
			}
			if strings.Contains(out, "--egress open") {
				t.Errorf("init recommends an unsupported runtime workaround:\n%s", out)
			}
			data, err := os.ReadFile(filepath.Join(dir, ".agent", "project.yaml"))
			if err != nil || !strings.Contains(string(data), "\n  egress: filtered\n") {
				t.Errorf("the committed policy must stay filtered on %s: %v\n%s", tc.runtime, err, data)
			}
		})
	}
}

// With no runtime chosen, init detects this machine's (reportUnfilteredRuntime). One runtime alone
// is chosen from PATH without running it: Apple's container gets the warning, Docker none, and no
// runtime at all is left to the install steps and coop doctor.
func TestInitDetectsTheRuntimeWhenNoneIsChosen(t *testing.T) {
	for _, tc := range []struct {
		name, installed string
		warns           bool
	}{{"Apple container alone", "container", true}, {"Docker alone", "docker", false}, {"no runtime", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			bin, ran := t.TempDir(), filepath.Join(t.TempDir(), "runtime-ran")
			if tc.installed != "" {
				stub := "#!/bin/sh\n: > '" + ran + "'\nexit 1\n"
				if err := os.WriteFile(filepath.Join(bin, tc.installed), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// The repository is made before PATH loses git; init then finds no git to set hooks with.
			hermeticGit(t)
			dir, cfgDir := t.TempDir(), t.TempDir()
			gitInit(t, dir)
			t.Setenv("PATH", bin)
			a := &app{cfg: &config.Config{RepoOverride: dir, ConfigDir: cfgDir, MCPFile: filepath.Join(cfgDir, "mcp.json")}}
			out := captureStderr(t, func() {
				if code, err := a.cmdInit([]string{"--services", "none", "--agents", "claude"}); code != 0 || err != nil {
					t.Fatalf("cmdInit = (%d, %v)", code, err)
				}
			})
			for _, line := range cannotFilterWarning {
				if strings.Contains(out, line) != tc.warns {
					t.Errorf("init output has %q = %t, want %t:\n%s", line, !tc.warns, tc.warns, out)
				}
			}
			if pathExists(ran) {
				t.Errorf("detection ran %s, which PATH alone should have chosen", tc.installed)
			}
		})
	}
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
					a := initAppOnDocker(repo, cfgDir)
					run := func() error {
						if api == "scaffold" {
							_, err := scaffold.Init(repo, stack, []string{"go"}, []string{"claude", "codex", "gemini"})
							return err
						}
						code, err := a.cmdInit([]string{"--stack", stack, "--services", "postgres", "--agents", "all"})
						reportExit(code, err) // the dispatcher draws a returned failure
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
	a := initAppOnDocker(repo, cfgDir)
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

// Services added to a fresh filtered project need one approval and no hand edits: coop init, then
// coop init --services, then coop approve grants each service by name and port. That grant is what
// lets a filtered launch reach the service instead of refusing (box.prepareFilteredExecution);
// reaching it over the gateway needs Docker, so the task's manual check covers that last step.
func TestInitServicesNeedOneApproval(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	hermeticGit(t)
	repo := t.TempDir()
	gitInit(t, repo)
	a := initAppOnDocker(repo, t.TempDir())
	for _, args := range [][]string{nil, {"--services", "postgres,redis"}} {
		captureStderr(t, func() {
			if code, err := a.cmdInit(args); code != 0 || err != nil {
				t.Fatalf("coop init %v = (%d, %v)", args, code, err)
			}
		})
	}
	// Fields only: formatting the access whole would call Error on its nil *PendingApproval.
	access, err := box.ProjectNetworkAccess(t.Context(), a.cfg, repo)
	if err != nil || access.Pending == nil {
		t.Fatalf("adding services asked for no approval (mode %s, err %v)", access.Mode, err)
	}
	review, err := box.ReviewProjectNetworkContext(t.Context(), a.cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(review.Commit(t.Context()), review.Close()); err != nil {
		t.Fatal(err)
	}
	access, err = box.ProjectNetworkAccess(t.Context(), a.cfg, repo)
	if err != nil || access.Pending != nil || access.Approval == nil {
		t.Fatalf("one approval left the project unapproved (pending %v, approved %v, err %v)", access.Pending != nil, access.Approval != nil, err)
	}
	for _, want := range []egress.Rule{
		{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}},
		{To: egress.Destination{Service: "redis"}, Protocol: "tcp", Ports: []int{6379}},
	} {
		if !slices.ContainsFunc(access.Approval.Envelope, func(got egress.Rule) bool { return reflect.DeepEqual(got, want) }) {
			t.Errorf("the approval does not grant %s: %+v", netRuleText(want), access.Approval.Envelope)
		}
	}
}

// Coop init edits .gitignore by writing a temporary copy beside it. When .coopignore hides
// .gitignore from the box, init stops before it writes anything, first run or re-run, names the
// file and says to stop hiding it.
func TestInitRefusesAHiddenGitignore(t *testing.T) {
	hermeticGit(t)
	for _, state := range []string{"first init", "re-init"} {
		t.Run(state, func(t *testing.T) {
			repo := t.TempDir()
			if state == "re-init" {
				repo = initializedProject(t)
			}
			for name, body := range map[string]string{".coopignore": ".gitignore\n", ".gitignore": "node_modules/\n"} {
				if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotInitTree(t, repo)
			a, _ := initApp(t, repo)
			var code int
			out := captureTerminal(t, func() { code = reportExit(a.cmdInit(nil)) })
			if code != 1 {
				t.Fatalf("init with a hidden .gitignore exited %d:\n%s", code, out)
			}
			for _, want := range []string{
				"Could not update .gitignore",
				"Coop will not edit .gitignore because it is hidden from the box.",
				"Stop hiding .gitignore in .coopignore, then run coop init again.",
				"No files were changed.",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "Setting up Coop") {
				t.Errorf("the refusal followed setup progress:\n%s", out)
			}
			if changed := initTreeChange(before, snapshotInitTree(t, repo)); changed != "" {
				t.Fatalf("refused init changed %s", changed)
			}
		})
	}
}

// A .coopignore that coop init cannot read stops it, and the refusal names that .coopignore and how
// to fix it, not the file init was editing: .gitignore before anything is written, the Compose file
// for --services, and .agent/project.yaml when a task queue is registered at the end of setup.
func TestInitNamesAnUnreadableCoopignore(t *testing.T) {
	hermeticGit(t)
	for _, tc := range []struct {
		name      string
		args      []string
		plant     func(t *testing.T, repo string)
		unchanged bool // the refusal comes before init writes anything
		want      []string
	}{
		{"coop init with a linked .coopignore", nil, func(t *testing.T, repo string) {
			if err := os.WriteFile(filepath.Join(repo, "rules"), []byte(".env\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("rules", filepath.Join(repo, ".coopignore")); err != nil {
				t.Fatal(err)
			}
		}, true, []string{
			"Could not read .coopignore",
			"Coop cannot tell what .coopignore hides, because it is a link.",
			"Remove .coopignore if you did not create it; otherwise make it a readable regular file of at most 1 MiB. Then run coop init again.",
			"No files were changed.",
		}},
		{"coop init --services with a FIFO .coopignore", []string{"--services", "postgres"}, func(t *testing.T, repo string) {
			if err := os.Mkdir(filepath.Join(repo, "config"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(repo, "config", ".coopignore"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("box:\n  compose: config/compose.yml\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, true, []string{
			"Could not read config/.coopignore",
			"Coop cannot tell what config/.coopignore hides, because it is not a regular file.",
			"Remove config/.coopignore if you did not create it; otherwise make it a readable regular file of at most 1 MiB. Then run coop init --services again.",
			"No services were changed.",
		}},
		{"task queue registration with a .coopignore over 1 MiB", nil, func(t *testing.T, repo string) {
			if err := os.MkdirAll(filepath.Join(repo, "api", ".agent"), 0o755); err != nil {
				t.Fatal(err)
			}
			padding := strings.Repeat("# padding\n", 1<<20/10+1)
			if err := os.WriteFile(filepath.Join(repo, ".agent", ".coopignore"), []byte(padding), 0o644); err != nil {
				t.Fatal(err)
			}
		}, false, []string{
			"Could not read .agent/.coopignore",
			"Coop cannot tell what .agent/.coopignore hides, because it is larger than 1 MiB.",
			"Remove .agent/.coopignore if you did not create it; otherwise make it a readable regular file of at most 1 MiB. Then run coop init again.",
			"Files already created have been kept.",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initializedProject(t)
			tc.plant(t, repo)
			before := snapshotInitTree(t, repo)
			a, _ := initApp(t, repo)
			var code int
			out := captureTerminal(t, func() { code = reportExit(a.cmdInit(tc.args)) })
			if code != 1 {
				t.Fatalf("exited %d:\n%s", code, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
			if changed := initTreeChange(before, snapshotInitTree(t, repo)); tc.unchanged && changed != "" {
				t.Fatalf("refused init changed %s", changed)
			}
		})
	}
}

// A box names the folders and files coop init reports, and can write project.yaml and the Git config
// init quotes, so any of them can hold terminal controls: an escape sequence that clears the screen,
// or a newline that starts a fake line of Coop's own. Init shows each one escaped.
func TestInitEscapesNamesABoxChose(t *testing.T) {
	hermeticGit(t)
	// name as a folder holds it, the opening of a YAML double-quoted string that spells it (each use
	// closes it after its own suffix), and name as init shows it.
	const name, yamlName, shown = "evil\x1b[2J\nfake", `"evil\e[2J\nfake`, `evil\u001B[2J\u000Afake`
	write := func(t *testing.T, repo, rel, body string) {
		t.Helper()
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	member := func(t *testing.T, repo string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(repo, name, ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		what  string
		args  []string
		plant func(t *testing.T, repo string)
		want  string
	}{
		{"a task queue it registers", nil, member, "Task queues registered: " + shown},
		{"a task queue it cannot register", nil, func(t *testing.T, repo string) {
			member(t, repo)
			write(t, repo, ".agent/project.yaml", "subprojects: [docs]\n") // a flow-style list coop's line edit cannot extend
		}, "Add " + shown + " to subprojects in .agent/project.yaml."},
		{"the project's own Dockerfile", nil, func(t *testing.T, repo string) {
			write(t, repo, name+"/Dockerfile", "FROM alpine\n")
		}, "Dockerfile: " + shown + "/Dockerfile"},
		{"box.dockerfile", nil, func(t *testing.T, repo string) {
			write(t, repo, name+"/Dockerfile", "FROM alpine\n")
			write(t, repo, ".agent/project.yaml", "box:\n  dockerfile: "+yamlName+"/Dockerfile\"\n")
		}, "Review " + shown + "/Dockerfile, then run:"},
		{"core.hooksPath", nil, func(t *testing.T, repo string) {
			gitInit(t, repo)
			if out, err := exec.Command("git", "-C", repo, "config", "core.hooksPath", name).CombinedOutput(); err != nil {
				t.Fatalf("git config: %v\n%s", err, out)
			}
		}, "core.hooksPath points to " + shown + "."},
		{"box.compose", []string{"--services", "postgres"}, func(t *testing.T, repo string) {
			write(t, repo, ".agent/project.yaml", "box:\n  compose: "+yamlName+"/compose.yml\"\n")
		}, "Service added to " + shown + "/compose.yml"},
		{"a box.compose it refuses", []string{"--services", "postgres"}, func(t *testing.T, repo string) {
			write(t, repo, name+"/compose.yml", "services: {}\n")
			write(t, repo, ".coopignore", "compose.yml\n")
			write(t, repo, ".agent/project.yaml", "box:\n  compose: "+yamlName+"/compose.yml\"\n")
		}, "Could not add services to " + shown + "/compose.yml"},
		{"a project.yaml it cannot check network access with", nil, func(t *testing.T, repo string) {
			write(t, repo, ".agent/project.yaml", yamlName+"\": true\n")
		}, "field " + shown + " not found"},
		{"the project's own Compose file", nil, func(t *testing.T, repo string) {
			write(t, repo, name+"/compose.yml", "services: {}\n")
		}, "Services: " + shown + "/compose.yml"},
		{"the project's own Compose file beside its Dockerfile", nil, func(t *testing.T, repo string) {
			write(t, repo, name+"/Dockerfile", "FROM alpine\n")
			write(t, repo, name+"/compose.yml", "services: {}\n")
		}, "Services:   " + shown + "/compose.yml"},
		{"a box.compose whose service name is taken", []string{"--services", "postgres"}, func(t *testing.T, repo string) {
			write(t, repo, name+"/compose.yml", "services:\n  db:\n    image: mysql:8\n")
			write(t, repo, ".agent/project.yaml", "box:\n  compose: "+yamlName+"/compose.yml\"\n")
		}, "Review " + shown + "/compose.yml before adding"},
		{"a box.compose that already has the service", []string{"--services", "postgres"}, func(t *testing.T, repo string) {
			write(t, repo, ".agent/project.yaml", "box:\n  compose: "+yamlName+"/compose.yml\"\n")
			repoApp, _ := initApp(t, repo)
			if code, err := repoApp.cmdInit([]string{"--services", "postgres"}); code != 0 || err != nil {
				t.Fatalf("first coop init --services = (%d, %v)", code, err)
			}
		}, "Services are already configured in " + shown + "/compose.yml."},
		{"a box.compose it cannot read", []string{"--services", "postgres"}, func(t *testing.T, repo string) {
			write(t, repo, name+"/compose.yml", "services: [\n")
			write(t, repo, ".agent/project.yaml", "box:\n  compose: "+yamlName+"/compose.yml\"\n")
		}, "Fix " + shown + "/compose.yml, then run coop init --services again."},
	} {
		t.Run(tc.what, func(t *testing.T) {
			repo := initializedProject(t)
			tc.plant(t, repo)
			a, _ := initApp(t, repo)
			var code int
			out := captureTerminal(t, func() { code = reportExit(a.cmdInit(tc.args)) })
			if !strings.Contains(out, tc.want) {
				t.Errorf("exited %d without %q:\n%s", code, tc.want, out)
			}
			if strings.Contains(out, "evil\x1b") || strings.Contains(out, "\nfake") {
				t.Errorf("printed the name unescaped:\n%s", out)
			}
		})
	}
}

// yaml.v3 lists one problem a line. An honest typo in project.yaml keeps its problem on a line of its
// own under the network-access warning, not escaped onto the warning's line.
func TestInitNetworkWarningKeepsEachYAMLProblemOnItsOwnLine(t *testing.T) {
	hermeticGit(t)
	repo := initializedProject(t)
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("fo: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := initApp(t, repo)
	out := captureTerminal(t, func() { _ = reportExit(a.cmdInit(nil)) })
	if !strings.Contains(out, "could not be checked: ") || !strings.Contains(out, "\n  line 1: field fo not found in type ") {
		t.Errorf("the yaml problem is not on a line of its own:\n%s", out)
	}
	if strings.Contains(out, `\u000A`) {
		t.Errorf("the warning escaped yaml's own line break:\n%s", out)
	}
}

// initTreeChange names an entry that differs between two snapshots of a tree, or is "" when none does.
func initTreeChange(before, after map[string]initTreeEntry) string {
	for name := range after {
		if _, ok := before[name]; !ok {
			return name
		}
	}
	for name, want := range before {
		got, ok := after[name]
		if !ok || !os.SameFile(want.info, got.info) || want.info.Mode() != got.info.Mode() || want.data != got.data || want.link != got.link {
			return name
		}
	}
	return ""
}

// The remedy follows the refusal: a file .coopignore hides is fixed by no longer hiding it, a path
// that leads to a hidden file or is not a regular file by fixing the path, anything else possibly by
// permissions.
func TestInitFixMatchesTheRefusal(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{scaffold.HiddenFile{File: ".agent/project.yaml"}, "Stop hiding .agent/project.yaml in .coopignore, then run coop init again."},
		{scaffold.HiddenFile{File: ".agent/project.yaml", LeadsTo: "secrets/project.yaml"}, "Fix the path, then run coop init again."},
		{fmt.Errorf("create: %w", scaffold.ErrNotRegular), "Fix the path, then run coop init again."},
		{fs.ErrPermission, "Fix the path or permissions, then run coop init again."},
	} {
		if got := initFix(tc.err); got != tc.want {
			t.Errorf("initFix(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// On an initialized project, --services together with --agents or --stack must still add the
// services: the re-init those options ask for writes nothing it would clobber, so it alone could
// never add a service or its network rule.
func TestInitServicesWithOtherOptionsStillAddServices(t *testing.T) {
	for _, extra := range [][]string{{"--agents", "claude"}, {"--stack", "asdf"}} {
		t.Run(extra[0], func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv(box.ServiceStateRootEnv, t.TempDir())
			hermeticGit(t)
			repo := t.TempDir()
			gitInit(t, repo)
			if extra[0] == "--stack" {
				if err := os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte("nodejs 22.11.0\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			a := initAppOnDocker(repo, t.TempDir())
			for _, args := range [][]string{nil, append([]string{"--services", "postgres"}, extra...)} {
				captureStderr(t, func() {
					if code, err := a.cmdInit(args); code != 0 || err != nil {
						t.Fatalf("coop init %v = (%d, %v)", args, code, err)
					}
				})
			}
			compose, err := os.ReadFile(filepath.Join(repo, ".agent", "compose.yml"))
			if err != nil || !strings.Contains(string(compose), "postgres") {
				t.Fatalf("--services postgres %s added no service (err %v):\n%s", extra[0], err, compose)
			}
			access, err := box.ProjectNetworkAccess(t.Context(), a.cfg, repo)
			if err != nil || access.Pending == nil {
				t.Fatalf("--services postgres %s asked for no service access (err %v)", extra[0], err)
			}
		})
	}
}
