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
