package cli

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/scaffold"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The approved `coop init` transcripts. The interactive first run is pinned in the task's own
// approved-output packet and re-verified at a real terminal; what runs here is everything a test
// can drive honestly — the non-interactive first run, the re-init results, the additive service
// operation, and the refusals.

// initApp is a project at a path the transcripts record, so the result line reads the same on
// every machine.
func initApp(t *testing.T, repo string) (*app, func(string) string) {
	t.Helper()
	cfgDir := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: cfgDir, MCPFile: filepath.Join(cfgDir, "mcp.json")}}
	return a, func(out string) string { return strings.ReplaceAll(out, repo, "/work/atlas") }
}

// settledProject is the state the re-init transcripts were approved against: a Git repository
// with an agent signed in, so the result has no outstanding setup job to list.
func settledProject(t *testing.T) (*app, func(string) string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// gitrepo.New closes both git config doors, so a developer's core.hooksPath or commit
	// template cannot change what init observes.
	repo, _ := gitrepo.New(t)
	a, normalize := initApp(t, repo)
	dir := a.cfg.AgentProfileDir("codex", a.cfg.DefaultProfileOf("codex"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return a, normalize, repo
}

// hermeticGit closes both git config doors for the whole test, so a developer's own
// core.hooksPath or commit template cannot change what a repository init observes here. It is
// gitrepo.New's contract, applied to the repositories `coop init` creates itself.
func hermeticGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
}

// signIn gives the config a stored credential for one agent, which is what decides whether the
// result names that agent, which vendor a filtered box may reach, and whether "Sign in" is still
// an outstanding job.
func signIn(t *testing.T, a *app, agent string) {
	t.Helper()
	dir := a.cfg.AgentProfileDir(agent, a.cfg.DefaultProfileOf(agent))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The complete first-run transcripts: coop's questions, the answers a person types, and the one
// result that follows from what this project really is. The typed answers come back through the
// same narrow seam a confirmation reads its reply from, and are echoed the way a terminal echoes
// them — coop never prints an answer itself.
func TestApprovedFirstRun(t *testing.T) {
	cases := []struct {
		fixture string
		args    []string
		answers []string
		files   map[string]string // what decides which questions init has to ask
		git     bool              // the folder is already a repository
	}{
		// Go is detected and Git is there, so the only question left is which services to run.
		{fixture: "13a-init-go-services", git: true,
			files:   map[string]string{"go.mod": "module atlas\n"},
			answers: typed("postgres redis")},
		// Nothing is detected: Git is offered and accepted, an unknown language is named and the
		// question repeats, and Enter at the services prompt means none.
		{fixture: "13b-init-git-yes-language-retry",
			answers: typed("y", "rustfmt", "rust", "")},
		// Declining Git creates nothing, and the result carries `git init` as the job that
		// finishes setup.
		{fixture: "13c-init-git-declined",
			answers: typed("n", "", "")},
		// Both lists supplied and a stack detected: nothing is asked, and the box Dockerfile
		// --stack wrote becomes the job to review before building.
		{fixture: "13e-init-stack-services-none", git: true,
			files: map[string]string{".tool-versions": "golang 1.26.5\n"},
			args:  []string{"--stack", "asdf", "--services", "none"}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			hermeticGit(t)
			repo := t.TempDir()
			if tc.git {
				gitInit(t, repo)
			}
			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			a, normalize := initApp(t, repo)
			signIn(t, a, "codex")
			typedAnswers(t, tc.answers...)
			var code int
			out := captureTerminal(t, func() { code, _ = a.cmdInit(tc.args) })
			if code != 0 {
				t.Fatalf("cmdInit = %d:\n%s", code, out)
			}
			assertApprovedSession(t, tc.fixture, normalize(out))
		})
	}
}

// A folder coop cannot write into stops the run with the operation that failed and the safe path
// it was working on — and says the files already written are still there, because they are.
func TestApprovedInitFailures(t *testing.T) {
	// Git said no, so coop says what Git said and changes nothing else.
	t.Run("13v-init-git-failed", func(t *testing.T) {
		hermeticGit(t)
		repo := t.TempDir()
		a, normalize := initApp(t, repo)
		signIn(t, a, "codex")
		typedAnswers(t, "y")
		// A read-only folder is the honest cause: git cannot create .git inside it.
		if err := os.Chmod(repo, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(repo, 0o700) })
		var code int
		var err error
		out := captureTerminal(t, func() { code, err = a.cmdInit(nil) })
		if code != 1 || !errors.Is(err, ui.ErrReported) {
			t.Fatalf("a failed git init = (%d, %v), want (1, a reported failure)", code, err)
		}
		assertApprovedSession(t, "13v-init-git-failed", normalize(out))
	})

	// A scaffold write that fails leaves real files behind. Claiming a rollback that did not
	// happen would be worse than the partial state. (A re-init: the missing file is being
	// restored, so there is no first-run header above the failure.)
	t.Run("13w-init-write-failed", func(t *testing.T) {
		repo := reinitProject(t)
		blocked := filepath.Join(repo, ".agent", "skills", "work")
		if err := os.RemoveAll(blocked); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(blocked, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
		a, _ := initApp(t, repo)
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit(nil) })
		if code != 1 {
			t.Fatalf("a blocked scaffold write exited %d, want 1:\n%s", code, out)
		}
		assertApprovedOutput(t, "13w-init-write-failed", out)
	})

	// An existing entry that is not a regular file is the person's; coop names it rather than
	// replacing it.
	t.Run("13x-init-non-regular-path", func(t *testing.T) {
		repo := reinitProject(t)
		skill := filepath.Join(repo, ".agent", "skills", "work", "SKILL.md")
		if err := os.Remove(skill); err != nil {
			t.Fatal(err)
		}
		// A link to nothing: the skill reads as missing, so coop tries to restore it, and the
		// entry in the way is not a regular file it may replace.
		if err := os.Symlink("gone", skill); err != nil {
			t.Fatal(err)
		}
		a, _ := initApp(t, repo)
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit(nil) })
		if code != 1 {
			t.Fatalf("a non-regular scaffold path exited %d, want 1:\n%s", code, out)
		}
		assertApprovedOutput(t, "13x-init-non-regular-path", out)
	})

	// The global MCP stub is outside the project, so its path is printed in full.
	t.Run("13y-init-mcp-stub-failed", func(t *testing.T) {
		repo := reinitProject(t)
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, "agents"), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, "agents"), 0o700) })
		a, _ := initApp(t, repo)
		a.cfg.MCPFile = filepath.Join(home, "agents", "mcp.json")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit(nil) })
		if code != 1 {
			t.Fatalf("a blocked MCP stub exited %d, want 1:\n%s", code, out)
		}
		got := strings.ReplaceAll(out, a.cfg.MCPFile, "/Users/alex/.config/coop/agents/mcp.json")
		assertApprovedOutput(t, "13y-init-mcp-stub-failed", got)
	})
}

// reinitProject is a project coop has already set up, ready for the re-init states: the writes a
// second run makes are restorations, so nothing asks and no first-run header is printed.
func reinitProject(t *testing.T) string {
	t.Helper()
	hermeticGit(t)
	repo := t.TempDir()
	gitInit(t, repo)
	a, _ := initApp(t, repo)
	if code, err := a.cmdInit(nil); code != 0 || err != nil {
		t.Fatalf("first init = (%d, %v)", code, err)
	}
	return repo
}

// gitInit makes repo a repository with the identity the hooks need, using the config doors
// hermeticGit already closed.
// gitInit makes repo a Git repository the test fully owns: the developer's global and system git
// config are pinned away, so commit.gpgsign, core.hooksPath, a commit template or an excludes file
// on the machine cannot change what these fixtures observe (see the hermetic-git-tests rule).
func gitInit(t *testing.T, repo string) {
	t.Helper()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "noglobal"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "nosystem"),
		"GIT_CONFIG_NOSYSTEM=1")
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "T"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// A project with no Git, no detectable language and nobody signed in: coop writes what it can,
// says what a filtered box may reach, and lists only the jobs this project's real state needs.
func TestApprovedInitWithoutATerminal(t *testing.T) {
	repo := t.TempDir()
	a, normalize := initApp(t, repo)
	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdInit(nil) })
	if code != 0 {
		t.Fatalf("cmdInit = %d:\n%s", code, out)
	}
	assertApprovedOutput(t, "13d-init-no-terminal", normalize(out))
}

func TestApprovedReinit(t *testing.T) {
	t.Run("13f-reinit", func(t *testing.T) {
		a, normalize, _ := settledProject(t)
		if code, err := a.cmdInit(nil); code != 0 || err != nil {
			t.Fatalf("first init = (%d, %v)", code, err)
		}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit(nil) })
		if code != 0 {
			t.Fatalf("re-init = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "13f-reinit", normalize(out))
	})

	// A member directory nobody listed is a task queue coop would silently ignore, so a re-init
	// registers it and says which ones it added.
	t.Run("13g-reinit-registers-queues", func(t *testing.T) {
		a, normalize, repo := settledProject(t)
		if code, err := a.cmdInit(nil); code != 0 || err != nil {
			t.Fatalf("first init = (%d, %v)", code, err)
		}
		for _, member := range []string{"api", "web"} {
			if err := os.MkdirAll(filepath.Join(repo, member, ".agent", "tasks"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit(nil) })
		if code != 0 {
			t.Fatalf("re-init = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "13g-reinit-registers-queues", normalize(out))
	})

	// A hand-restructured project.yaml is the person's file. Coop will not rewrite it: it says
	// exactly what is missing and where to put it.
	t.Run("13h-reinit-unregistered-queues", func(t *testing.T) {
		a, normalize, repo := settledProject(t)
		if code, err := a.cmdInit(nil); code != 0 || err != nil {
			t.Fatalf("first init = (%d, %v)", code, err)
		}
		for _, member := range []string{"api", "web"} {
			if err := os.MkdirAll(filepath.Join(repo, member, ".agent", "tasks"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		// A flow-style list is valid YAML that coop's surgical line edit cannot extend safely.
		if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"),
			[]byte("subprojects: [docs]\nbox:\n  egress: filtered\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit(nil) })
		if code != 0 {
			t.Fatalf("re-init = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "13h-reinit-unregistered-queues", normalize(out))
	})
}

// A project that already has its own Docker gets a pointer to it, not an adopted file: coop
// suggests the setting, and the person decides.
func TestApprovedDockerSetupSuggestion(t *testing.T) {
	cases := []struct {
		fixture string
		files   map[string]string
	}{
		{"13k-docker-setup-both", map[string]string{
			"Dockerfile":  "FROM alpine\n",
			"compose.yml": "services:\n  db:\n    image: postgres\n"}},
		{"13k1-docker-setup-dockerfile", map[string]string{"Dockerfile": "FROM alpine\n"}},
		{"13k2-docker-setup-compose", map[string]string{"compose.yml": "services:\n  db:\n    image: postgres\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			a, normalize, repo := settledProject(t)
			if code, err := a.cmdInit(nil); code != 0 || err != nil {
				t.Fatalf("first init = (%d, %v)", code, err)
			}
			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var code int
			out := captureTerminal(t, func() { code, _ = a.cmdInit(nil) })
			if code != 0 {
				t.Fatalf("re-init = %d:\n%s", code, out)
			}
			assertApprovedSession(t, tc.fixture, normalize(out))
		})
	}
}

// Two hook compositions coop will not take over: it says what it kept, and leaves the person the
// one step only they can take.
func TestApprovedReinitHookNotices(t *testing.T) {
	t.Run("13i-reinit-custom-hookspath", func(t *testing.T) {
		a, normalize, repo := settledProject(t)
		if code, err := a.cmdInit(nil); code != 0 || err != nil {
			t.Fatalf("first init = (%d, %v)", code, err)
		}
		// The project points Git somewhere else. Coop's hooks are written, but activating them
		// would silently disable the ones already in use.
		if out, err := exec.Command("git", "-C", repo, "config", "core.hooksPath", "hooks").CombinedOutput(); err != nil {
			t.Fatalf("git config: %v\n%s", err, out)
		}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit(nil) })
		if code != 0 {
			t.Fatalf("re-init = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "13i-reinit-custom-hookspath", normalize(out))
	})

	t.Run("13j-reinit-custom-prepare-hook", func(t *testing.T) {
		a, normalize, repo := settledProject(t)
		if code, err := a.cmdInit(nil); code != 0 || err != nil {
			t.Fatalf("first init = (%d, %v)", code, err)
		}
		hook := filepath.Join(repo, ".githooks", "prepare-commit-msg")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\n# the project's own\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit(nil) })
		if code != 0 {
			t.Fatalf("re-init = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "13j-reinit-custom-prepare-hook", normalize(out))
	})
}

// A project.yaml that keeps changing under the registration is the one setup failure with its own
// remedy: wait for the other edit. Every other registration failure is an ordinary setup failure.
func TestApprovedInitRegistrationRetriesExhausted(t *testing.T) {
	out := captureTerminal(t, func() { _ = registrationFailure(scaffold.ErrProjectChanged) })
	assertApprovedOutput(t, "13z-init-registration-retries", out)
}

func TestApprovedServiceChooser(t *testing.T) {
	// The chooser on a project that has no Compose file yet: the answer creates one, and the
	// result names the Compose service each catalog entry became.
	t.Run("13l-services-chooser", func(t *testing.T) {
		repo := initializedProject(t)
		a, _ := initApp(t, repo)
		typedAnswers(t, "postgres redis")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit([]string{"--services"}) })
		if code != 0 {
			t.Fatalf("chooser = %d:\n%s", code, out)
		}
		assertApprovedSession(t, "13l-services-chooser", out)
	})

	// Enter is an answer: nothing is added, and nothing is written.
	t.Run("13n-services-chooser-none", func(t *testing.T) {
		repo := initializedProject(t)
		a, _ := initApp(t, repo)
		typedAnswers(t, "")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit([]string{"--services"}) })
		if code != 0 {
			t.Fatalf("chooser = %d:\n%s", code, out)
		}
		assertApprovedSession(t, "13n-services-chooser-none", out)
		if _, err := os.Stat(filepath.Join(repo, ".agent", "compose.yml")); err == nil {
			t.Error("an empty answer wrote a Compose file")
		}
	})

	// The mistyped answer from the chooser, kept as the shorter path through the same prompt.
	t.Run("13m-services-unknown-then-redis", func(t *testing.T) {
		repo := initializedProject(t)
		a, _ := initApp(t, repo)
		typedAnswers(t, "mysql", "redis")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit([]string{"--services"}) })
		if code != 0 {
			t.Fatalf("chooser = %d:\n%s", code, out)
		}
		assertApprovedSession(t, "13m-services-unknown-then-redis", out)
	})

	// The edit lands in the Compose file the project actually configured, and a service already
	// there is reported under the name it really has, not added twice.
	t.Run("13p-services-custom-path", func(t *testing.T) {
		repo := customComposeProject(t, "services:\n  db:\n    image: postgres:18\n")
		a, _ := initApp(t, repo)
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit([]string{"--services", "postgres,redis"}) })
		if code != 0 {
			t.Fatalf("add to a custom path = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "13p-services-custom-path", trimLeadingBlank(out))
	})

	// A Compose file coop cannot read is named with the line to fix, and nothing is written.
	t.Run("13t-services-malformed-compose", func(t *testing.T) {
		// A service line with no colon: the parser stops on it, and its line is the one to fix.
		before := "# dev services\nservices:\n  db:\n    image: postgres:18\n    ports:\n      - \"5432:5432\"\n\n  redis\n"
		repo := customComposeProject(t, before)
		a, _ := initApp(t, repo)
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit([]string{"--services", "redis"}) })
		if code != 1 {
			t.Errorf("a malformed Compose file exited %d, want 1", code)
		}
		assertApprovedOutput(t, "13t-services-malformed-compose", out)
		if after, _ := os.ReadFile(filepath.Join(repo, "infra", "dev-compose.yml")); string(after) != before {
			t.Errorf("a refused add changed the Compose file:\n%s", after)
		}
	})

	// An unknown answer is NAMED and the same question repeats: silently dropping half an answer
	// leaves the person believing they chose something they did not. (The complete chooser
	// transcript needs a terminal; it is verified there, and the rejection itself is here.)
	t.Run("an unknown service is named and the question repeats", func(t *testing.T) {
		var chosen []string
		out := captureTerminal(t, func() {
			chosen = promptExactTokens(bufio.NewScanner(strings.NewReader("mysql\nredis\n")), []string{
				"Choose services to run alongside your agents.",
				"Existing services and their data will be kept.",
			}, scaffold.ComposeServices, "Services", unknownServiceBlock)
		})
		if len(chosen) != 1 || chosen[0] != "redis" {
			t.Errorf("chooser = %v, want the retry's answer [redis]", chosen)
		}
		for _, want := range []string{
			"  postgres\n  redis\n",
			"Services (space-separated, or press Enter for none): ",
			"✗ Unknown service \"mysql\"\n\n      Choose from: postgres, redis\n",
			"\nServices: ",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("chooser output missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "mysql\n\nServices:") {
			t.Error("the rejected answer was accepted into the menu")
		}
	})

	// --services none is an answer, not a no-op: it says so and writes nothing.
	t.Run("13o-services-none", func(t *testing.T) {
		repo := initializedProject(t)
		a, _ := initApp(t, repo)
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit([]string{"--services", "none"}) })
		if code != 0 {
			t.Fatalf("--services none = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "13o-services-none", strings.TrimPrefix(out, "\n"))
	})

	// Adding what is already there changes nothing and says which names hold them.
	t.Run("13q-services-already-configured", func(t *testing.T) {
		repo := initializedProject(t)
		a, _ := initApp(t, repo)
		if code, err := a.cmdInit([]string{"--services", "postgres,redis"}); code != 0 || err != nil {
			t.Fatalf("first add = (%d, %v)", code, err)
		}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit([]string{"--services", "postgres,redis"}) })
		if code != 0 {
			t.Fatalf("repeat add = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "13q-services-already-configured", strings.TrimPrefix(out, "\n"))
	})

	// Nobody can answer a chooser without a terminal, so it names the explicit form instead.
	t.Run("13r-services-needs-a-terminal", func(t *testing.T) {
		repo := initializedProject(t)
		a, _ := initApp(t, repo)
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit([]string{"--services"}) })
		if code != 2 {
			t.Errorf("a chooser without a terminal exited %d, want 2", code)
		}
		assertApprovedOutput(t, "13r-services-needs-a-terminal", out)
	})

	// A service name the project already uses for something else is the project's. Coop refuses
	// rather than renaming or replacing it, and changes nothing.
	t.Run("13s-services-name-collision", func(t *testing.T) {
		repo := initializedProject(t)
		compose := filepath.Join(repo, ".agent", "compose.yml")
		before := "services:\n  db:\n    image: mysql:8\n"
		if err := os.WriteFile(compose, []byte(before), 0o644); err != nil {
			t.Fatal(err)
		}
		a, _ := initApp(t, repo)
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdInit([]string{"--services", "postgres"}) })
		if code != 1 {
			t.Errorf("a name collision exited %d, want 1", code)
		}
		assertApprovedOutput(t, "13s-services-name-collision", out)
		if after, _ := os.ReadFile(compose); string(after) != before {
			t.Errorf("a refused add changed the Compose file:\n%s", after)
		}
	})
}

// A --stack coop cannot honor is refused before it says a word or asks a question.
func TestApprovedInitStackRefusal(t *testing.T) {
	repo := t.TempDir()
	a, normalize := initApp(t, repo)
	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdInit([]string{"--stack", "asdf"}) })
	if code != 1 {
		t.Errorf("a stack with no .tool-versions exited %d, want 1", code)
	}
	assertApprovedOutput(t, "13u-stack-missing-tool-versions", normalize(out))
}

// customComposeProject is an initialized project whose services live somewhere other than the
// default path — the state that proves coop edits the file the project configured, and names it.
func customComposeProject(t *testing.T, compose string) string {
	t.Helper()
	repo := initializedProject(t)
	if err := os.MkdirAll(filepath.Join(repo, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "infra", "dev-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"),
		[]byte("box:\n  compose: infra/dev-compose.yml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// initializedProject is a project coop has already set up, which is the only state the additive
// service operation applies to.
func initializedProject(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	a, _ := initApp(t, repo)
	if code, err := a.cmdInit(nil); code != 0 || err != nil {
		t.Fatalf("init = (%d, %v)", code, err)
	}
	return repo
}
