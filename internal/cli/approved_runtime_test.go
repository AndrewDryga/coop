package cli

import (
	"strings"
	"testing"
)

// TestApprovedRuntimeHelpPages pins the runtime and integration family's help: run, shell, ACP,
// every sessions page, sign, completion, prompt and version. Each page is read from the SAME
// source the manual and `coop <cmd> --help` read, so terminal, `coop help --all` and docs/cli.md
// cannot disagree about a page's bytes.
func TestApprovedRuntimeHelpPages(t *testing.T) {
	cases := []struct{ fixture, command string }{
		{"14-run-help", "run"},
		{"15-shell-help", "shell"},
		{"67-acp-help", "acp"},
		{"69-sessions-help", "sessions"},
		{"69-sessions-serve-help", "sessions serve"},
		{"69-sessions-doctor-help", "sessions doctor"},
		{"69-sessions-policies-help", "sessions policies"},
		{"69-sessions-compact-help", "sessions compact"},
		{"73-sessions-connect-help", "sessions connect"},
		{"74-sign-help", "sign"},
		{"75-completion-help", "completion"},
		{"76-prompt-help", "prompt"},
		{"77-version-help", "version"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			assertApprovedPage(t, tc.fixture, manualPage(tc.command))
		})
	}
}

// Every page above is also what `coop help <path>` prints, with no all-commands footer: these
// pages end with their own pointer or need none, so the generic footer would be a second answer.
func TestApprovedRuntimeHelpRoutes(t *testing.T) {
	cfg := freshConfig(t)
	for _, path := range [][]string{
		{"run"}, {"shell"}, {"acp"}, {"sign"}, {"completion"}, {"prompt"}, {"version"},
		{"sessions"}, {"sessions", "connect"}, {"sessions", "serve"},
		{"sessions", "doctor"}, {"sessions", "policies"}, {"sessions", "compact"},
	} {
		name := strings.Join(path, " ")
		t.Run(name, func(t *testing.T) {
			out := captureStdout(t, func() {
				if code, err := helpForPath(path, cfg, true); code != 0 || err != nil {
					t.Fatalf("coop help %s exited (%d, %v)", name, code, err)
				}
			})
			if strings.Contains(out, "Run 'coop help' for all commands.") {
				t.Errorf("coop help %s appended the generic all-commands footer:\n%s", name, out)
			}
			if want := manualPage(name); !strings.Contains(out, want) {
				t.Errorf("coop help %s did not print its manual page:\n%s", name, out)
			}
		})
	}
}

// `coop worker` is retired: its workflow is `coop sessions connect`. Nothing may re-offer the old
// spelling — not the dispatch, not help, not the manual, not shell completion.
func TestWorkerCommandIsRetired(t *testing.T) {
	if slicesContains(topLevelCommands, "worker") {
		t.Error("worker is still dispatched")
	}
	if slicesContains(manualOrder, "worker") {
		t.Error("worker still has a page in the manual")
	}
	if _, ok := commandHelp["worker"]; ok {
		t.Error("worker still has a help page")
	}
	a := &app{cfg: freshConfig(t), argv: []string{"__complete"}}
	for _, c := range a.completionCandidates(nil) {
		if c == "worker" {
			t.Error("worker is still a completion candidate")
		}
	}
	code, err := a.dispatch([]string{"worker", "connect", "--config", "/tmp/worker.json"})
	if err == nil {
		t.Fatalf("coop worker was accepted (exit %d)", code)
	}
	if !strings.Contains(err.Error(), `Unknown command "coop worker"`) {
		t.Errorf("coop worker should be refused as an unknown command, got: %v", err)
	}
}

func slicesContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
