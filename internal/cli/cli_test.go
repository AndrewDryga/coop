package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestRejectArgs(t *testing.T) {
	if err := rejectArgs("build", nil); err != nil {
		t.Errorf("no args should be ok, got %v", err)
	}
	err := rejectArgs("build", []string{"help"})
	if err == nil {
		t.Fatal("an unexpected arg should error")
	}
	if s := err.Error(); !strings.Contains(s, "coop build") || !strings.Contains(s, "--help") {
		t.Errorf("error should name the command and point to --help: %q", s)
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
	if s := string(out); !strings.Contains(s, "coop build") || !strings.Contains(s, "Usage: coop build") {
		t.Errorf("`coop build help` should print build's help; got:\n%s", s)
	}
}

// TestMainBarePrintsHelp verifies bare `coop` prints help and exits 0 without a
// container runtime (it returns before runtime detection) — so a stray `coop`
// never launches an agent; running one is explicit (`coop claude`).
func TestMainBarePrintsHelp(t *testing.T) {
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
	if s := string(out); !strings.Contains(s, "Usage") || !strings.Contains(s, "coop <agent>") {
		t.Errorf("bare coop should print help listing `coop <agent>`; got:\n%s", s)
	}
}

// `coop help <cmd>` shows that command's help (≡ `coop <cmd> --help`), and `coop help <unknown>`
// is a usage error (exit 2) — the help arg used to be ignored, always printing the top-level help.
func TestMainHelpSubcommand(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	codeBuild := Main([]string{"help", "build"}) // == coop build --help, no runtime needed
	codeFork := helpForCommand("fork")           // the fork family help
	codeClaude := helpForCommand("claude")       // a known agent → points at its own --help
	codeBogus := helpForCommand("bogus")         // unknown → usage error (to stderr)
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if codeBuild != 0 || !strings.Contains(string(out), "Usage: coop build") {
		t.Errorf("`coop help build` = %d; want 0 + build's help, got:\n%s", codeBuild, out)
	}
	if codeFork != 0 {
		t.Errorf("helpForCommand(fork) = %d, want 0", codeFork)
	}
	if codeClaude != 0 {
		t.Errorf("helpForCommand(claude) = %d, want 0", codeClaude)
	}
	if codeBogus != 2 {
		t.Errorf("helpForCommand(bogus) = %d, want 2 (unknown command)", codeBogus)
	}
}

// `coop version` takes no arguments — extras are a usage error (exit 2), like every other no-arg
// command, not silently ignored.
func TestVersionRejectsExtraArgs(t *testing.T) {
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
	codeHelp := helpForCommand("help")
	codeVer := helpForCommand("version")
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if codeHelp != 0 || codeVer != 0 {
		t.Errorf("helpForCommand help=%d version=%d, want 0/0", codeHelp, codeVer)
	}
	if s := string(out); strings.Contains(s, "forwards --help") {
		t.Errorf("help/version must not print the broken passthrough pointer:\n%s", s)
	}
	if s := string(out); !strings.Contains(s, "Usage") {
		t.Errorf("`coop help help` should print the top-level reference:\n%s", s)
	}
}

// unknownErr is the one shape for a rejected subcommand/agent/value, with a typo hint for a
// near-miss. The sub-command groups (tasks/fleet/pool/profiles) all use it.
func TestUnknownErr(t *testing.T) {
	if got := unknownErr("tasks command", "bogus", []string{"list", "lint"}).Error(); got != `unknown tasks command "bogus" — use: list, lint` {
		t.Errorf("unknownErr = %q", got)
	}
	// A ≥4-char near-miss gets a "did you mean".
	if got := unknownErr("fleet command", "prnue", []string{"prune", "up"}).Error(); !strings.Contains(got, `did you mean "prune"`) {
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

// v3 retires renamed-command aliases: each retired form exits 2 with a tombstone naming the rewrite,
// via the one removedCommandNote registry.
func TestV3RetiredForms(t *testing.T) {
	for _, key := range []string{"clone", "pool", "tasks start", "loop --debug", "profiles", "profiles verb"} {
		if note, ok := removedCommandNote(key); !ok || note == "" {
			t.Errorf("removedCommandNote(%q) missing a tombstone", key)
		}
	}
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	// clone, top-level pool, and profiles (renamed to credentials) tombstone through dispatch
	// (no runtime needed — they fall to default).
	for _, argv := range [][]string{{"clone", "x"}, {"pool", "add", "p"}, {"profiles"}, {"profiles", "claude"}} {
		code, err := a.dispatch(argv)
		if code != 2 || err == nil {
			t.Errorf("dispatch(%v) = (%d, %v), want (2, a tombstone)", argv, code, err)
		}
	}
	// The profiles tombstone names the replacement command.
	if _, err := a.dispatch([]string{"profiles"}); err == nil || !strings.Contains(err.Error(), "coop credentials") {
		t.Errorf("profiles tombstone should point at coop credentials, got: %v", err)
	}
	// verb-first credential edits → tombstone (path grammar is the replacement).
	if code, err := a.cmdCredentials([]string{"rm", "claude", "work"}); code != 2 || err == nil {
		t.Errorf("verb-first credentials rm = (%d, %v), want (2, a tombstone)", code, err)
	}
	// tasks start → tombstone (renamed to claim).
	if code, err := cmdTasksFolder("", t.TempDir(), []string{"start", "x"}); code != 2 || err == nil {
		t.Errorf("tasks start = (%d, %v), want (2, a tombstone)", code, err)
	}
}

// `coop help <agent>` documents coop's OWN wrapper flags (the agent's real --help forwards elsewhere).
func TestHelpForAgentShowsWrapperFlags(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := helpForCommand("claude")
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if code != 0 {
		t.Fatalf("helpForCommand(claude) = %d, want 0", code)
	}
	for _, want := range []string{"[:model][@account]", "--preset", "--consult", "coop claude -- --help"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("agent help missing %q:\n%s", want, out)
		}
	}
}

// `coop status` and `coop help status` must show the SAME removal tombstone — one source, both paths.
func TestRemovedCommandNoteParity(t *testing.T) {
	note, ok := removedCommandNote("status")
	if !ok || !strings.Contains(note, "was removed") {
		t.Fatalf("removedCommandNote(status) = (%q, %v), want the tombstone", note, ok)
	}
	if _, ok := removedCommandNote("tasks"); ok {
		t.Error("a live command must not carry a removal note")
	}
	// `coop status` path: unknownCommandErr returns the note verbatim.
	if err := unknownCommandErr([]string{"status"}); err == nil || err.Error() != note {
		t.Errorf("unknownCommandErr(status) = %v, want the tombstone verbatim", err)
	}
	// `coop help status` path: helpForCommand prints the same note (stderr) and exits 2 — NOT the
	// generic "unknown command" (which would also be exit 2, hence the text check).
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	code := helpForCommand("status")
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if code != 2 || !strings.Contains(string(out), "was removed") {
		t.Errorf("helpForCommand(status) = exit %d, out=%q; want 2 + the tombstone", code, out)
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
