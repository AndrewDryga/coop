package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

func useEmptyMainConfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coop.conf")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONF", path)
}

// The fallback error line can quote project.yaml, which a box can write: yaml quotes a value it
// could not use, and an unknown key, as written. The line escapes what would act on the terminal,
// a newline inside a quoted key does not start a line of its own, and the line breaks and tabs an
// error puts there itself stay.
func TestFallbackErrorLineEscapesBoxChosenText(t *testing.T) {
	color := regexp.MustCompile("\x1b\\[[0-9;]*m")
	for _, tc := range []struct{ name, file, want string }{
		{"a value", "serve:\n  ports: [\"evil\\x1b[2J\\nfake\"]\n", "cannot unmarshal !!str `evil\\u001B[2...` into int"},
		{"a key", "\"evil\\x1b[2J\\nfake\": 1\n", "field evil\\u001B[2J\\u000Afake not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := project.Parse([]byte(tc.file))
			if err == nil {
				t.Fatal("project.yaml parsed")
			}
			out := color.ReplaceAllString(captureStderr(t, func() { reportExit(-1, err) }), "")
			if strings.ContainsRune(out, '\x1b') || !strings.Contains(out, tc.want) || strings.Count(out, "\n") != 2 {
				t.Errorf("error line = %q, want it escaped on yaml's two lines with %q", out, tc.want)
			}
		})
	}
	planted := fmt.Errorf("read task: %w", &fs.PathError{Op: "open", Path: "/repo/evil\nfake", Err: fs.ErrPermission})
	if out := color.ReplaceAllString(captureStderr(t, func() { reportExit(-1, planted) }), ""); out != "✗ read task: open /repo/evil\\u000Afake: permission denied\n" {
		t.Errorf("file error = %q, want the file name's newline escaped", out)
	}
	out := color.ReplaceAllString(captureStderr(t, func() { reportExit(-1, errors.New("could not commit\n\tgit: index.lock exists")) }), "")
	if out != "✗ could not commit\n\tgit: index.lock exists\n" {
		t.Errorf("multi-line error = %q, want its own line break and tab kept", out)
	}
}

func TestMainRejectsInvalidConfigBeforeRuntime(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "coop.conf")
	if err := os.WriteFile(conf, []byte("COOP_HOMES=flase\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "runtime-called")
	runtime := filepath.Join(t.TempDir(), "runtime")
	script := "#!/bin/sh\n: > \"" + marker + "\"\n"
	if err := os.WriteFile(runtime, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONF", conf)
	t.Setenv("COOP_RUNTIME", runtime)

	stderr := captureStderr(t, func() {
		if code := Main([]string{"build"}); code != 1 {
			t.Errorf("Main invalid config exit = %d, want 1", code)
		}
	})
	if !strings.Contains(stderr, conf+":1") || !strings.Contains(stderr, "COOP_HOMES") {
		t.Fatalf("invalid config stderr = %q, want path:line and key", stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("runtime marker stat = %v, want runtime untouched", err)
	}
}

func TestRejectArgs(t *testing.T) {
	if err := rejectArgs("build", nil); err != nil {
		t.Errorf("no args should be ok, got %v", err)
	}
	err := rejectArgs("build", []string{"help"})
	if err == nil {
		t.Fatal("an unexpected arg should error")
	}
	// It names the first extra token, the command it was given to, and that command's own page.
	if s := err.Error(); !strings.Contains(s, `Unexpected argument "help" for "coop build"`) || !strings.Contains(s, "coop help build") {
		t.Errorf("error should name the argument, the command and its help: %q", s)
	}
}

// The stdout "views" must gate color on stdout (ui.For(os.Stdout)), not on stderr (the package-level
// ui.Bold/ui.Dim), so `coop profiles | grep` / `coop help | cat` from an interactive shell get clean
// text. In `go test` both streams are non-tty, so this locks the non-tty-clean invariant; it can't
// reproduce the stderr-tty/stdout-pipe split without a pty (that repro is in the task log). fork ls
// needs a live fork to print its header, so it's covered by review — its header uses the same one-liner.
func TestStdoutViewsNoANSI(t *testing.T) {
	if s := helpText(&config.Config{}); strings.ContainsRune(s, '\x1b') {
		t.Errorf("helpText leaked ESC:\n%q", s)
	}
	capture := func(fn func()) string {
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		fn()
		_ = w.Close()
		os.Stdout = old
		out, _ := io.ReadAll(r)
		return string(out)
	}
	a := modelsApp(t)
	views := map[string]func(){
		"commandHelp": func() { printCommandHelp(commandHelp["tasks"]) },
		"models":      func() { _, _ = a.cmdModels(nil) },
		"profiles":    func() { _, _ = a.cmdCredentials(nil) },
	}
	for name, fn := range views {
		if out := capture(fn); strings.ContainsRune(out, '\x1b') {
			t.Errorf("%s leaked ESC into piped stdout:\n%q", name, out)
		}
	}
}

func TestMainCommandHelpArg(t *testing.T) {
	useEmptyMainConfig(t)
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := Main([]string{"build", "help"})
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if code != 0 {
		t.Errorf("`coop build help` exit = %d, want 0", code)
	}
	if s := string(out); !strings.Contains(s, "coop build: build the Coop box image") || !strings.Contains(s, "Usage:\n  coop build") {
		t.Errorf("`coop build help` should print build's help; got:\n%s", s)
	}
}

// TestMainBarePrintsHelp verifies bare `coop` prints help and exits 0 without a
// container runtime (it returns before runtime detection) — so a stray `coop`
// never launches an agent; running one is explicit (`coop claude`).
func TestMainBarePrintsHelp(t *testing.T) {
	useEmptyMainConfig(t)
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := Main(nil)
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if code != 0 {
		t.Errorf("bare coop exit = %d, want 0", code)
	}
	if s := string(out); !strings.Contains(s, "Usage") || !strings.Contains(s, "coop <target>") {
		t.Errorf("bare coop should print help listing `coop <target>`; got:\n%s", s)
	}
}

// `coop help <cmd> [<sub>]` shows that command's page (≡ `coop <cmd> [<sub>] --help`), and
// `coop help <unknown>` is a usage error (exit 2) — the help arg used to be ignored, always
// printing the top-level menu.
func TestMainHelpSubcommand(t *testing.T) {
	useEmptyMainConfig(t)
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	codeBuild := Main([]string{"help", "build"}) // == coop build --help, no runtime needed
	cfg := &config.Config{}
	codeFork, _ := helpForPath([]string{"fork"}, cfg, true)          // the fork family help
	codeClaude, _ := helpForPath([]string{"claude"}, cfg, true)      // coop's own page for the agent
	codeLeaf, _ := helpForPath([]string{"tasks", "add"}, cfg, true)  // a real leaf resolves
	codeBogus, bogusErr := helpForPath([]string{"bogus"}, cfg, true) // unknown → usage error
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if codeBuild != 0 || !strings.Contains(string(out), "Usage:\n  coop build") {
		t.Errorf("`coop help build` = %d; want 0 + build's help, got:\n%s", codeBuild, out)
	}
	if codeFork != 0 {
		t.Errorf("helpForPath(fork) = %d, want 0", codeFork)
	}
	if codeClaude != 0 || !strings.Contains(string(out), "coop claude: run Claude") {
		t.Errorf("helpForPath(claude) = %d; want 0 + coop's own Claude page, got:\n%s", codeClaude, out)
	}
	if codeLeaf != 0 {
		t.Errorf("helpForPath(tasks add) = %d, want 0", codeLeaf)
	}
	if codeBogus != 2 || bogusErr == nil {
		t.Errorf("helpForPath(bogus) = (%d, %v), want (2, unknown-command error)", codeBogus, bogusErr)
	}
}

// `coop version` takes no arguments — extras are a usage error (exit 2), like every other no-arg
// command, not silently ignored.
func TestVersionRejectsExtraArgs(t *testing.T) {
	useEmptyMainConfig(t)
	oo, oe := os.Stdout, os.Stderr
	_, w, _ := os.Pipe()
	os.Stdout, os.Stderr = w, w
	codeExtra := Main([]string{"version", "foo"})
	codeOK := Main([]string{"version"})
	_ = w.Close()
	os.Stdout, os.Stderr = oo, oe
	if codeExtra != 2 {
		t.Errorf("Main(version foo) = %d, want 2 (usage error)", codeExtra)
	}
	if codeOK != 0 {
		t.Errorf("Main(version) = %d, want 0", codeOK)
	}
}

// `coop help help` / `coop help version` must not print a broken "forwards --help — run 'coop help
// --help'" pointer (neither has an underlying CLI, and `coop help --help` errors). help prints the
// top-level reference; version a synopsis.
func TestHelpForHelpAndVersion(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cfg := &config.Config{}
	codeHelp, _ := helpForPath([]string{"help"}, cfg, true)
	codeVer, _ := helpForPath([]string{"version"}, cfg, true)
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if codeHelp != 0 || codeVer != 0 {
		t.Errorf("helpForPath help=%d version=%d, want 0/0", codeHelp, codeVer)
	}
	if s := string(out); strings.Contains(s, "forwards --help") {
		t.Errorf("help/version must not print the broken passthrough pointer:\n%s", s)
	}
	if s := string(out); !strings.Contains(s, "Usage") {
		t.Errorf("`coop help help` should print the top-level reference:\n%s", s)
	}
}

// Pure-local families work with NO container runtime; only box-running commands surface the runtime
// error. Detect is lazy (a.ensureRuntime), not eager in Main — so install→init→browse the queue and
// CI `coop tasks lint` don't require Docker.
func TestRuntimeDetectIsLazy(t *testing.T) {
	// No codex either: `coop models` refreshes a due catalog, and Codex's comes from its CLI.
	t.Setenv("PATH", t.TempDir())
	bogus := func() *app {
		return &app{cfg: &config.Config{RuntimeName: "coop-no-such-runtime-xyz", ConfigDir: t.TempDir()}}
	}
	if err := bogus().ensureRuntime(); err == nil {
		t.Fatal("ensureRuntime with a bogus runtime should error")
	}
	// A box-running command (dispatched) hits the runtime error up front...
	if code, err := bogus().dispatch([]string{"build"}); err == nil || code == 0 {
		t.Errorf("coop build with no runtime should fail, got (%d, %v)", code, err)
	}
	// ...but a pure-local one never detects. Capture stdout so the listing stays quiet.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := bogus().dispatch([]string{"models"})
	_ = w.Close()
	os.Stdout = old
	_, _ = io.ReadAll(r)
	if code != 0 || err != nil {
		t.Errorf("coop models with no runtime should succeed (pure-local), got (%d, %v)", code, err)
	}
}

// The exit-code contract is coop's machine interface (CI/scripts branch on it): 0 success · 1 failure
// or findings · 2 usage. Pin representative cases so it can't drift silently (documented in docs/configuration.md).
func TestExitCodeContract(t *testing.T) {
	a := modelsApp(t)
	a.cfg.RepoOverride = t.TempDir()
	// 2 — usage: an unknown command, and bad arguments.
	if code, _ := a.dispatch([]string{"nonesuch-xyz"}); code != 2 {
		t.Errorf("unknown command exit = %d, want 2", code)
	}
	if code, _ := a.dispatch([]string{"fork", "rm", "a", "b"}); code != 2 {
		t.Errorf("bad-arguments exit = %d, want 2", code)
	}
	// 1 — failure: rm of a task that doesn't exist.
	if code, _ := cmdTasksFolder("", t.TempDir(), []string{"rm", "no-such-task", "--yes"}); code != 1 {
		t.Errorf("failure (no such task) exit = %d, want 1", code)
	}
	// 0 — success: a pure-local listing (stdout discarded).
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, _ := a.dispatch([]string{"models"})
	_ = w.Close()
	os.Stdout = old
	_, _ = io.ReadAll(r)
	if code != 0 {
		t.Errorf("ok command exit = %d, want 0", code)
	}
}

// A final review that cannot find a task's one commit refuses in the shared failure block: the count
// it found, where it looked, and how to repair the history, never a bare error line.
func TestPendingReviewCommitsRefusalIsAFailureBlock(t *testing.T) {
	base := strings.Repeat("ab", 20)
	for _, tc := range []struct {
		found   int
		commits []string
		cause   string
		remedy  string
	}{
		{0, nil, "found 0", "Restore one commit with that trailer, then run the loop again."},
		{2, []string{"1a2b3c4 by Worker <w@x>", "5d6e7f8 by Andrew <a@x>"}, "found 2: 1a2b3c4 by Worker <w@x>, 5d6e7f8 by Andrew <a@x>",
			"Drop the extra commit or squash them, then run the loop again."},
	} {
		err := fmt.Errorf("recover pending final review: %w", &tasks.PendingReviewCommitsError{TaskID: "fix-login", Base: base, Found: tc.found, Commits: tc.commits})
		got := usageBlock(t, err)
		want := "\n✗ Final review cannot continue\n\n" +
			"      Task fix-login needs exactly one commit with \"Coop-Task: fix-login\" after " + base[:12] +
			", where this run started; " + tc.cause + ".\n\n" +
			"  " + tc.remedy + "\n"
		if got != want {
			t.Errorf("found %d:\n--- got ---\n%s\n--- want ---\n%s", tc.found, got, want)
		}
		if asUsage(ui.Reported(err)) != nil {
			t.Error("a refusal the loop already drew would be drawn again")
		}
	}
}

// v3 keeps no renamed-command aliases: a retired form is just an unknown command/subcommand now
// (exit 2), not a special "X is retired" note. Locked in against a future re-mint.
func TestV3RetiredForms(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	// clone, top-level pool, and profiles (renamed to credentials) fall through to the plain
	// unknown-command error (exit 2) via dispatch — no runtime needed.
	for _, argv := range [][]string{{"clone", "x"}, {"pool", "add", "p"}, {"profiles"}, {"profiles", "claude"}} {
		code, err := a.dispatch(argv)
		if code != 2 || err == nil {
			t.Errorf("dispatch(%v) = (%d, %v), want (2, unknown-command error)", argv, code, err)
		}
	}
	// A leading verb-first credential edit reads as an unknown agent now (exit 2).
	if code, err := a.cmdCredentials([]string{"rm", "claude", "work"}); code != 2 || err == nil {
		t.Errorf("verb-first credentials rm = (%d, %v), want (2, error)", code, err)
	}
	// tasks start → unknown tasks command (renamed to claim).
	if code, err := cmdTasksFolder("", t.TempDir(), []string{"start", "x"}); code != 2 || err == nil {
		t.Errorf("tasks start = (%d, %v), want (2, unknown tasks command)", code, err)
	}
}

// `coop help claude` prints the page generated from Claude's adapter, to the byte, and no generic
// all-commands footer: the page ends with its own pointer. docs/cli.md pins the page's bytes
// (make docs-check); this pins the help ROUTE that prints it.
func TestHelpForAgentPrintsItsPage(t *testing.T) {
	var code int
	out := captureStdout(t, func() { code, _ = helpForPath([]string{"claude"}, &config.Config{}, true) })
	if code != 0 {
		t.Fatalf("helpForPath(claude) = %d, want 0", code)
	}
	if want := agentHelp("claude") + "\n"; out != want {
		t.Errorf("coop help claude printed:\n%s\nwant the generated page:\n%s", out, want)
	}
}

// Every other agent gets the SAME page generated from its own adapter: its command, its human
// name, its argument label — and an effort in a page's example must be one that agent accepts.
func TestHelpForAgentIsGeneratedPerAdapter(t *testing.T) {
	codex := agentHelp("codex")
	for _, want := range []string{
		"coop codex: run Codex in the box",
		"coop codex[:<model>][/<effort>][@<account>] [options] [-- <codex-args>...]",
		"coop codex:gpt-6-astra",
		"pass all remaining arguments directly to Codex",
		"coop models codex       list Codex models",
	} {
		if !strings.Contains(codex, want) {
			t.Errorf("codex page missing %q:\n%s", want, codex)
		}
	}
	gemini := agentHelp("gemini")
	if !strings.Contains(gemini, "coop gemini[:<model>][/<effort>][@<account>]") {
		t.Errorf("gemini takes an effort, so its usage must show one:\n%s", gemini)
	}
	geminiAgent, _ := agents.Get("gemini")
	example := "coop gemini:" + geminiAgent.ExampleModel() + "/high@work"
	if !strings.Contains(gemini, example) {
		t.Errorf("gemini page lacks its effort example %q:\n%s", example, gemini)
	}
	if target, err := agents.ParseTarget(strings.TrimPrefix(example, "coop ")); err != nil {
		t.Errorf("gemini page's example %q is refused: %v (%v)", example, err, target)
	}
	// The rows still line up on the widest cell, whatever the agent's name length.
	for _, name := range agents.Names() {
		for _, line := range strings.Split(agentHelp(name), "\n") {
			if strings.HasPrefix(line, "  coop models ") && !strings.Contains(line, "   list ") {
				t.Errorf("%s: models row lost its column gap: %q", name, line)
			}
		}
	}
}

// `coop fork --help` must match every other command's template: a Usage line under the title and
// the standard footer (it used to have neither).
func TestForkHelpTemplate(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, _ := forkHelp("")
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	s := string(out)
	if code != 0 {
		t.Errorf("forkHelp exit = %d, want 0", code)
	}
	if !strings.Contains(s, "Usage: coop fork <name> [<target|preset>] [<options>]") {
		t.Errorf("fork help missing a Usage line:\n%s", s)
	}
	// The page ends with its own next step, so it never gets the generic all-commands footer.
	if !strings.HasSuffix(strings.TrimRight(s, "\n"), "Command options: coop help fork <command>") {
		t.Errorf("fork help should end with its own pointer:\n%s", s)
	}
}
