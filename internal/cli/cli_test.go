package cli

import (
	"io"
	"os"
	"strings"
	"testing"
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

// TestMainCommandHelpArg verifies `coop <cmd> help` prints that command's help (like
// --help) and exits 0 without a runtime — it returns from the help block before runtime
// detection, so a bare `help` arg is never silently swallowed and the build never runs.
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
	if s := string(out); !strings.Contains(s, "Usage") || !strings.Contains(s, "coop claude") {
		t.Errorf("bare coop should print help listing `coop claude`; got:\n%s", s)
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
