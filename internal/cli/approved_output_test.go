package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// fixtureVersion is the build version recorded in the approved menu transcripts. It is the one
// value substituted before a comparison: a fixture pins the COPY, not the tag it was captured on.
const fixtureVersion = "v9.0.0-187-g1176bf4-dirty"

// assertApprovedOutput compares rendered output byte-for-byte with testdata/approved/<name>.txt —
// the human-approved transcript of that invocation. A mismatch means the RENDERER drifted from an
// approved design decision; see the README beside the fixtures before touching one.
func assertApprovedOutput(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "approved", name+".txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read approved fixture: %v", err)
	}
	want := strings.ReplaceAll(string(data), fixtureVersion, resolveVersion())
	if got == want {
		return
	}
	t.Errorf("%s does not match the approved output %s\n--- got ---\n%s\n--- want ---\n%s\n--- first difference ---\n%s",
		name, path, got, want, firstDifference(got, want))
}

// firstDifference names the first line that differs, so a byte-exact failure is readable.
func firstDifference(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		gl, wl := "", ""
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return "line " + strconv.Itoa(i+1) + ":\n  got:  " + quote(gl) + "\n  want: " + quote(wl)
		}
	}
	return "(no line differs — check the trailing newline)"
}

func quote(s string) string { return "\"" + s + "\"" }

// usageBlock renders a rejected-input error exactly as a plain (NO_COLOR, piped) terminal shows it.
func usageBlock(t *testing.T, err error) string {
	t.Helper()
	usage := asUsage(err)
	if usage == nil {
		t.Fatalf("error is not a shared usage refusal: %#v", err)
	}
	return usage.Render(ui.Palette{})
}

// signedInConfig is a config whose agent has a stored credential, so the menu shows the normal
// form; freshConfig has none, so it shows GET STARTED. Both point at an EMPTY project, which is
// the no-configured-services state.
func signedInConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := freshConfig(t)
	dir := cfg.AgentProfileDir("claude", "default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !anyAgentSignedIn(cfg) {
		t.Fatal("fixture config should report a signed-in agent")
	}
	return cfg
}

func freshConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir(), BoxHome: t.TempDir()}
}

// withServices declares postgres and redis in the project's Compose file, the state the
// service-variant menus were approved against.
func withServices(t *testing.T, cfg *config.Config) *config.Config {
	t.Helper()
	dir := filepath.Join(cfg.RepoOverride, ".agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  postgres:\n    image: postgres:17\n  redis:\n    image: redis:7\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestApprovedMainMenu pins the four approved states of `coop` / `coop help`: signed in or not,
// with configured services or not. Rendered plain, which is what NO_COLOR and a pipe produce.
func TestApprovedMainMenu(t *testing.T) {
	cases := []struct {
		fixture string
		cfg     func(*testing.T) *config.Config
	}{
		{"01-main-help", signedInConfig},
		{"02a-first-run-help", freshConfig},
		{"02b-configured-services-help", func(t *testing.T) *config.Config { return withServices(t, signedInConfig(t)) }},
		{"02b-first-run-configured-services-help", func(t *testing.T) *config.Config { return withServices(t, freshConfig(t)) }},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			assertApprovedOutput(t, tc.fixture, renderMenu(ui.Palette{}, tc.cfg(t), false))
		})
	}
}

// TestApprovedInputErrors drives each rejected invocation through its REAL parser and pins the
// block it renders. Stdout stays empty and the exit status is 2 wherever the command returns one.
func TestApprovedInputErrors(t *testing.T) {
	newApp := func(t *testing.T) *app { return &app{cfg: freshConfig(t), argv: []string{"claude"}} }
	queue := func(t *testing.T) string { return t.TempDir() }

	cases := []struct {
		fixture string
		run     func(*testing.T) error
	}{
		{"03a-unknown-command-suggestion", func(*testing.T) error {
			return unknownCommandErr([]string{"doctro"}, false)
		}},
		{"03a-unknown-command-no-suggestion", func(*testing.T) error {
			return unknownCommandErr([]string{"something"}, false)
		}},
		{"03a-unknown-help-topic-suggestion", func(t *testing.T) error {
			code, err := helpForPath([]string{"doctro"}, freshConfig(t), true)
			return codeErr(t, code, err)
		}},
		{"03b-unknown-subcommand-suggestion", func(t *testing.T) error {
			code, err := cmdTasksFolder("", queue(t), []string{"watxh"})
			return codeErr(t, code, err)
		}},
		{"03b-unknown-subcommand-no-suggestion", func(t *testing.T) error {
			code, err := cmdTasksFolder("", queue(t), []string{"something"})
			return codeErr(t, code, err)
		}},
		{"03b-unknown-help-subcommand-suggestion", func(t *testing.T) error {
			code, err := helpForPath([]string{"tasks", "watxh"}, freshConfig(t), true)
			return codeErr(t, code, err)
		}},
		{"03c-unknown-option-suggestion", func(t *testing.T) error {
			code, err := newApp(t).cmdModels([]string{"--refesh"})
			return codeErr(t, code, err)
		}},
		{"03c-unknown-option-no-suggestion", func(t *testing.T) error {
			code, err := newApp(t).cmdModels([]string{"--something"})
			return codeErr(t, code, err)
		}},
		{"03d-missing-option-value", func(t *testing.T) error {
			code, err := newApp(t).cmdInit([]string{"--agents"})
			return codeErr(t, code, err)
		}},
		{"03e-invalid-choice", func(t *testing.T) error {
			code, err := newApp(t).cmdInit([]string{"--agents", "cursor"})
			return codeErr(t, code, err)
		}},
		{"03e-invalid-number", func(*testing.T) error {
			_, _, _, _, _, _, _, err := parseLoopArgs([]string{"--max-tasks", "0"}, false)
			return err
		}},
		{"03f-missing-title", func(t *testing.T) error {
			code, err := cmdTasksFolder("", queue(t), []string{"add"})
			return codeErr(t, code, err)
		}},
		{"03f-missing-task-id", func(t *testing.T) error {
			code, err := cmdTasksFolder("", queue(t), []string{"claim"})
			return codeErr(t, code, err)
		}},
		{"03g-extra-argument-no-args-command", func(*testing.T) error {
			return rejectArgs("version", []string{"extra"})
		}},
		{"03g-extra-argument-single-arg-command", func(t *testing.T) error {
			code, err := newApp(t).cmdLogin([]string{"claude", "codex"})
			return codeErr(t, code, err)
		}},
		{"03h-conflicting-options", func(*testing.T) error {
			_, _, err := extractExposureFlags("coop claude", []string{"--readonly", "--bare"})
			return err
		}},
		{"03h-standalone-value", func(t *testing.T) error {
			code, err := newApp(t).cmdInit([]string{"--agents", "all,claude"})
			return codeErr(t, code, err)
		}},
		{"03h-repeated-option", func(*testing.T) error {
			_, _, err := extractExposureFlags("coop claude", []string{"--readonly", "--readonly"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			err := tc.run(t)
			if err == nil {
				t.Fatal("invocation was accepted; the approved refusal is untested")
			}
			assertApprovedOutput(t, tc.fixture, usageBlock(t, err))
		})
	}
}

// codeErr checks that a rejected invocation exits 2 (the usage status) and returns its error.
func codeErr(t *testing.T, code int, err error) error {
	t.Helper()
	if err != nil && code != 2 {
		t.Errorf("rejected input exited %d, want 2", code)
	}
	return err
}

// A usage refusal writes only to stderr, and its plain form carries no escape sequences.
func TestUsageErrorIsPlainOnStderr(t *testing.T) {
	err := unknownCommandErr([]string{"doctro"}, false)
	var usage *ui.UsageError
	if !errors.As(err, &usage) {
		t.Fatal("unknown command should be a shared usage refusal")
	}
	if plain := usage.Render(ui.Palette{}); strings.Contains(plain, "\x1b") {
		t.Errorf("plain render must have no escape sequences: %q", plain)
	}
	colored := usage.Render(ui.Colored())
	if !strings.Contains(colored, "\x1b[31m✗ Unknown command") {
		t.Errorf("a terminal render must lead with a red headline: %q", colored)
	}
	if strings.Contains(colored, "\x1b[31m  Did you mean") {
		t.Errorf("guidance stays in the normal foreground: %q", colored)
	}
}
