package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func useEmptyMainConfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coop.conf")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONF", path)
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
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
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
	if s := string(out); !strings.Contains(s, "coop build — build the Coop box image") || !strings.Contains(s, "Usage:\n  coop build") {
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
	if codeClaude != 0 || !strings.Contains(string(out), "coop claude — run Claude") {
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

// unknownErr is the shape for a rejected VALUE (an agent name, a credential attribute), with a
// typo hint for a near-miss. Rejected commands and options use the approved blocks instead.
func TestUnknownErr(t *testing.T) {
	if got := unknownErr("agent", "bogus", []string{"claude", "codex"}).Error(); got != `unknown agent "bogus" — use: claude, codex` {
		t.Errorf("unknownErr = %q", got)
	}
	// A ≥4-char near-miss gets a "did you mean".
	if got := unknownErr("agent", "codexx", []string{"claude", "codex"}).Error(); !strings.Contains(got, `did you mean "codex"`) {
		t.Errorf("expected a suggestion in: %q", got)
	}
}

// Pure-local families work with NO container runtime; only box-running commands surface the runtime
// error. Detect is lazy (a.ensureRuntime), not eager in Main — so install→init→browse the queue and
// CI `coop tasks lint` don't require Docker.
func TestRuntimeDetectIsLazy(t *testing.T) {
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
// or findings · 2 usage. Pin representative cases so it can't drift silently (documented in README).
func TestExitCodeContract(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}}
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

// `coop help claude` is the approved page, to the byte: how to run Claude, the coop flags read
// before a --, where its models and accounts live, and one pointer to presets. Pinned whole
// because every line of it was chosen — a "contains" test would let the essay grow back.
func TestHelpForAgentIsTheApprovedPage(t *testing.T) {
	const want = `coop claude — run Claude in a sandboxed box

Usage:
  coop claude[:<model>][/<effort>][@<account>] [options] [-- <claude-args>...]

Examples
  coop claude
  coop claude:opus
  coop claude:opus/high@work
  coop claude -- --help

Options
  --peer <target>  start with a read-only peer agent; repeat to add more
  --readonly       mount the repository read-only
  --bare           run without the repository, project context, or tools
  --               pass all remaining arguments directly to Claude

  --readonly and --bare cannot be combined or used with peers.

Models and accounts
  coop models claude        list Claude models
  coop credentials claude   list Claude accounts
  coop login claude         sign in to Claude

For a guide to using multiple models and providers together:
  coop help presets
`
	var code int
	out := captureStdout(t, func() { code, _ = helpForPath([]string{"claude"}, &config.Config{}, true) })
	if code != 0 {
		t.Fatalf("helpForPath(claude) = %d, want 0", code)
	}
	if out != want {
		t.Errorf("coop help claude drifted from the approved page:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
	// No generic all-commands footer: the page ends with its own pointer.
	if strings.Contains(out, "Run 'coop help' for all commands") {
		t.Errorf("agent help should not append the all-commands footer:\n%s", out)
	}
}

// Every other agent gets the SAME page generated from its own adapter: its command, its human
// name, its argument label — and only the flags it accepts. Codex refuses restricted runs today,
// so its page must not advertise --readonly/--bare (nor the sentence about combining them), and
// Gemini has no reasoning effort, so its usage carries no /<effort>.
func TestHelpForAgentIsGeneratedPerAdapter(t *testing.T) {
	codex := agentHelp("codex")
	for _, want := range []string{
		"coop codex — run Codex in a sandboxed box",
		"coop codex[:<model>][/<effort>][@<account>] [options] [-- <codex-args>...]",
		"coop codex:gpt-5.6-sol",
		"pass all remaining arguments directly to Codex",
		"coop models codex        list Codex models",
	} {
		if !strings.Contains(codex, want) {
			t.Errorf("codex page missing %q:\n%s", want, codex)
		}
	}
	for _, unsupported := range []string{"--readonly", "--bare", "cannot be combined"} {
		if strings.Contains(codex, unsupported) {
			t.Errorf("codex page advertises %q, which a codex run refuses:\n%s", unsupported, codex)
		}
	}
	if gemini := agentHelp("gemini"); strings.Contains(gemini, "/<effort>") || strings.Contains(gemini, "/high") {
		t.Errorf("gemini has no reasoning effort, so its page must not show one:\n%s", gemini)
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
	code, _ := forkHelp()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	s := string(out)
	if code != 0 {
		t.Errorf("forkHelp exit = %d, want 0", code)
	}
	if !strings.Contains(s, "  Usage: coop fork ") {
		t.Errorf("fork help missing a Usage line:\n%s", s)
	}
	if !strings.HasSuffix(strings.TrimRight(s, "\n"), "Run 'coop help' for all commands.") {
		t.Errorf("fork help should end with the standard footer:\n%s", s)
	}
}
