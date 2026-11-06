package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// helpUsageLine is the first syntax line of a command's help page, whether the page writes it beside
// "Usage:" or on the next line.
func helpUsageLine(t *testing.T, page string) string {
	t.Helper()
	_, after, ok := strings.Cut(manualPage(page), "\nUsage:")
	if !ok {
		t.Fatalf("coop help %s has no Usage line", page)
	}
	line, _, _ := strings.Cut(strings.TrimLeft(after, " \n"), "\n")
	return line
}

// Every command refuses a token it did not take by one rule: a token that starts with "-" is an
// unknown option, any other is an unexpected argument, and that refusal's Usage line is the one
// `coop help <command>` prints. Each row gives the arguments the command accepts before the bad one.
func TestArgumentRefusalsShareOneRule(t *testing.T) {
	cases := []struct {
		page string
		lead []string
		run  func(a *app, args []string) (int, error)
	}{
		{"completion", []string{"bash"}, func(_ *app, args []string) (int, error) { return cmdCompletion(args) }},
		{"shell", nil, (*app).cmdShell},
		{"up", nil, (*app).cmdUp},
		{"check-secrets", []string{"--include-ignored"}, (*app).cmdCheckSecrets},
		{"build", []string{"--egress", "open"}, (*app).cmdBuild},
		{"down", []string{"--yes"}, (*app).cmdDown},
		{"approve", []string{"--case-exposure"}, (*app).cmdApprove},
		{"net runs", []string{"--json"}, (*app).cmdNetRuns},
		{"login", []string{"claude"}, (*app).cmdLogin},
		{"version", nil, func(_ *app, args []string) (int, error) { return 2, rejectArgs("version", args) }},
		{"doctor", nil, (*app).cmdDoctor},
		{"prompt", nil, (*app).cmdPrompt},
		{"sign", []string{"--from", "origin/main"}, (*app).cmdSign},
		{"init", []string{"--stack", "asdf"}, (*app).cmdInit},
		{"update", []string{"--check"}, (*app).cmdUpdate},
		{"eval inspect", []string{"run-1"}, (*app).evalInspect},
		{"fork", []string{"api", "claude"}, (*app).cmdFork},
	}
	for _, tc := range cases {
		command := "coop " + tc.page
		usage := helpUsageLine(t, tc.page)
		for _, bad := range []struct{ arg, want string }{
			{"--bogus", fmt.Sprintf("Unknown option %q for %q — See available options: %s", "--bogus", command, ui.HelpCommand(command))},
			{"extra", fmt.Sprintf("Unexpected argument %q for %q — Usage: %s — Help: %s", "extra", command, usage, ui.HelpCommand(command))},
		} {
			args := append(append([]string{}, tc.lead...), bad.arg)
			a := &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}, argv: append([]string{tc.page}, args...)}
			code, err := tc.run(a, args)
			if code != 2 || err == nil || err.Error() != bad.want {
				t.Errorf("%s %s = (%d, %v), want (2, %q)", command, strings.Join(args, " "), code, err, bad.want)
			}
		}
	}
}

// A shell name is the one thing coop completion takes, so a second word or any option is refused as
// such, never reported as a missing shell.
func TestCompletionRefusesExtraArguments(t *testing.T) {
	const usage = "coop completion <bash|zsh>"
	for args, want := range map[string]string{
		"":                 `Missing shell for "coop completion" — Usage: ` + usage + ` — Help: coop help completion`,
		"bash zsh":         `Unexpected argument "zsh" for "coop completion" — Usage: ` + usage + ` — Help: coop help completion`,
		"bash --bogus":     `Unknown option "--bogus" for "coop completion" — See available options: coop help completion`,
		"--bogus":          `Unknown option "--bogus" for "coop completion" — See available options: coop help completion`,
		"--bogus bash zsh": `Unknown option "--bogus" for "coop completion" — See available options: coop help completion`,
		"fish":             `Invalid value "fish" for "shell" in "coop completion" — Choose bash or zsh. — Example: coop completion zsh — Help: coop help completion`,
	} {
		var code int
		var err error
		out := captureStdout(t, func() { code, err = cmdCompletion(strings.Fields(args)) })
		if code != 2 || err == nil || err.Error() != want || out != "" {
			t.Errorf("coop completion %s = (%d, %v) printing %q, want (2, %q) and no script", args, code, err, out, want)
		}
	}
}

// coop run takes everything after its options as the command, so it has no extra argument to
// refuse; its missing-command refusal still shows the syntax its help page does.
func TestRunRefusalsMatchItsHelp(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}, argv: []string{"run"}}
	want := `Missing command for "coop run" — Usage: ` + helpUsageLine(t, "run") + ` — Help: coop help run`
	if code, err := a.cmdRun(nil); code != 2 || err == nil || err.Error() != want {
		t.Errorf("coop run = (%d, %v), want (2, %q)", code, err, want)
	}
	want = `Unknown option "--bogus" for "coop run" — See available options: coop help run`
	if code, err := a.cmdRun([]string{"--bogus", "--", "true"}); code != 2 || err == nil || err.Error() != want {
		t.Errorf("coop run --bogus -- true = (%d, %v), want (2, %q)", code, err, want)
	}
}
