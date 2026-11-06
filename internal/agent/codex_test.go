package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestCodexLaunchArgs(t *testing.T) {
	for _, c := range []struct {
		name                 string
		typed, native, wants []string
	}{
		{name: "nothing typed"},
		{name: "interactive flag", typed: []string{"--search"}, wants: []string{"--search"}},
		{name: "prompt after -p", typed: []string{"-p", "go"}, wants: []string{"exec", "--", "go"}},
		{name: "-p with =", typed: []string{"-p=go"}, wants: []string{"exec", "--", "go"}},
		{name: "-p joined", typed: []string{"-pfix it"}, wants: []string{"exec", "--", "fix it"}},
		{name: "one-word prompt that names a subcommand", typed: []string{"-p", "help"}, wants: []string{"exec", "--", "help"}},
		{name: "no prompt reaches codex's own error", typed: []string{"-p"}, wants: []string{"exec"}},
		{name: "flag between -p and its prompt", typed: []string{"-p", "--json", "go"}, wants: []string{"exec", "--json", "--", "go"}},
		{name: "flag between -p and a one-word prompt", typed: []string{"-p", "--model", "m", "help"}, wants: []string{"exec", "--model", "m", "--", "help"}},
		{name: "flags on both sides", typed: []string{"--model", "m", "-p", "go", "--json"}, wants: []string{"exec", "--model", "m", "--json", "--", "go"}},
		{name: "repeatable flag cannot take the prompt", typed: []string{"-i", "a.png", "b.png", "-p", "go"}, wants: []string{"exec", "-i", "a.png", "b.png", "--", "go"}},
		{name: "after -p, -i still takes its words as codex does", typed: []string{"-p", "-i", "a.png", "go"}, wants: []string{"exec", "-i", "a.png", "go"}},
		{name: "flag only exec takes", typed: []string{"-o", "out.txt", "-p", "go"}, wants: []string{"exec", "-o", "out.txt", "--", "go"}},
		{name: "value that looks like a subcommand", typed: []string{"-C", "e", "-p", "go"}, wants: []string{"exec", "-C", "e", "--", "go"}},
		{name: "arguments past --", typed: []string{"-p", "go"}, native: []string{"--json"}, wants: []string{"exec", "--json", "--", "go"}},
		{name: "exec's help past --", typed: []string{"-p"}, native: []string{"--help"}, wants: []string{"exec", "--help"}},
		{name: "-p past -- is codex's", native: []string{"-p", "work"}, wants: []string{"-p", "work"}},
		{name: "--profile is codex's", typed: []string{"--profile", "work"}, wants: []string{"--profile", "work"}},
		{name: "-p after exec is codex's", typed: []string{"exec", "-p", "work", "go"}, wants: []string{"exec", "-p", "work", "go"}},
		{name: "-p after the exec alias is codex's", typed: []string{"e", "-p", "work", "go"}, wants: []string{"e", "-p", "work", "go"}},
		{name: "-p after a flagged exec is codex's", typed: []string{"--model", "m", "exec", "-p", "work", "go"}, wants: []string{"--model", "m", "exec", "-p", "work", "go"}},
		{name: "-p after resume still picks a profile", typed: []string{"resume", "--last", "-p", "work"}, wants: []string{"resume", "--last", "-p", "work"}},
		{name: "-p inside another subcommand", typed: []string{"sandbox", "mkdir", "-p", "d"}, wants: []string{"sandbox", "mkdir", "-p", "d"}},
		{name: "a prompt mentioning -p", typed: []string{"explain -p"}, wants: []string{"explain -p"}},
	} {
		if got := (codexAgent{}).LaunchArgs(c.typed, c.native); !slices.Equal(got, c.wants) {
			t.Errorf("%s: LaunchArgs(%q, %q) = %q, want %q", c.name, c.typed, c.native, got, c.wants)
		}
	}
	// The rewritten form must be one the session lock reads as headless.
	if (codexAgent{}).ProducesSession((codexAgent{}).LaunchArgs([]string{"--model", "m", "-p", "go"}, nil)) {
		t.Error("coop codex -p should not take the interactive session lock")
	}
}

// Both managed layers Coop gives Codex, the image's and the API-key broker's mounted over it, keep
// the background server off: with it on, the first interactive start in a home copies the whole
// client into it and runs an updater. Codex refuses a managed file it cannot parse, and a second
// [features] table would be one.
func TestCodexManagedLayersKeepTheBackgroundServerOff(t *testing.T) {
	var layers []SystemFile
	for _, file := range (codexAgent{}).UpdateControls().Files {
		if file.Path == codexManagedConfig {
			layers = append(layers, file)
		}
	}
	layers = append(layers, (codexAgent{}).CredentialBroker().Config(BrokerBases{Model: "https://coop-broker:15580/v1"}))
	if len(layers) != 2 {
		t.Fatalf("managed layers = %d, want the image's and the broker's", len(layers))
	}
	for i, layer := range layers {
		var managed struct {
			CheckForUpdateOnStartup *bool           `toml:"check_for_update_on_startup"`
			ModelProvider           string          `toml:"model_provider"`
			Features                map[string]bool `toml:"features"`
		}
		if layer.Path != codexManagedConfig {
			t.Fatalf("managed layer %d is written to %s, want %s", i, layer.Path, codexManagedConfig)
		}
		if err := toml.Unmarshal([]byte(layer.Content), &managed); err != nil {
			t.Fatalf("managed layer %d does not parse: %v\n%s", i, err, layer.Content)
		}
		daemon, set := managed.Features["daemon_auto_start"]
		if managed.CheckForUpdateOnStartup == nil || *managed.CheckForUpdateOnStartup || !set || daemon {
			t.Errorf("managed layer leaves an updater on:\n%s", layer.Content)
		}
		if i == 1 { // the broker's layer still selects its own provider over plain POSTs
			if websockets, set := managed.Features["responses_websockets"]; managed.ModelProvider != "coop-broker" || !set || websockets {
				t.Errorf("broker layer lost its provider:\n%s", layer.Content)
			}
		}
	}
}

func TestCodexModelsMenu(t *testing.T) {
	want := []string{
		"gpt-6.1-sol",
		"gpt-6-astra",
		"gpt-6-sol",
		"gpt-6-luna",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.5",
	}
	if got := (codexAgent{}).Models(); !slices.Equal(got, want) {
		t.Fatalf("codex Models() = %v, want current list-visible catalog %v", got, want)
	}
}

// TestCodexPeerRowShell runs the ACTUAL generated codex_peer_row (from ShellPrelude) against a
// real-shaped `codex exec --json` turn.completed event and asserts it appends one peer-usage row:
// input_tokens as "in", output_tokens+reasoning_output_tokens as "out". With no COOP_RUN_ID (not a
// loop) it must write and say nothing. A named ledger it cannot use is reported once per box, and
// the shell still succeeds. Skipped where jq is absent (the box always has it).
func TestCodexPeerRowShell(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	// The turn.completed shape verified against a live `codex exec --json` run (2026-07-13).
	stream := `{"type":"turn.completed","usage":{"input_tokens":15880,"cached_input_tokens":9984,"output_tokens":5,"reasoning_output_tokens":0}}`
	row := "printf '%s\\n' \"$1\" | codex_peer_row thinker gpt-5.6-terra\n"
	script := codexAgent{}.ShellPrelude() + "\n" + row

	// run returns the ledger's content and everything the shell printed. calls repeats the append in
	// one box (one TMPDIR), as several consultations in one task attempt do.
	run := func(runID string, calls int, setup ...func(string, string)) (string, string) {
		dir := t.TempDir()
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("init fixture repo: %v\n%s", err, out)
		}
		if err := os.MkdirAll(filepath.Join(dir, ".agent", "runs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if runID != "" {
			path := filepath.Join(dir, ".agent", "runs", runID+".peers.jsonl")
			if len(setup) > 0 {
				setup[0](dir, path)
			} else if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		nested := filepath.Join(dir, "portal")
		if err := os.Mkdir(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", script+strings.Repeat(row, calls-1), "sh", stream)
		cmd.Dir = nested
		cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
		if runID != "" {
			cmd.Env = append(cmd.Env, "COOP_RUN_ID="+runID)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("shell failed: %v\n%s", err, out)
		}
		b, _ := os.ReadFile(filepath.Join(dir, ".agent", "runs", runID+".peers.jsonl"))
		return strings.TrimSpace(string(b)), string(out)
	}

	got, out := run("r1", 1)
	for _, want := range []string{`"run":"r1"`, `"role":"thinker"`, `"provider":"codex"`, `"model":"gpt-5.6-terra"`, `"in":15880`, `"out":5`} {
		if !strings.Contains(got, want) {
			t.Errorf("peer row missing %q: %q", want, got)
		}
	}
	if out != "" {
		t.Errorf("a recorded row printed %q", out)
	}
	if got, out := run("", 1); got != "" || out != "" {
		t.Errorf("no COOP_RUN_ID must write and say nothing, got row %q and output %q", got, out)
	}
	unsafe := map[string]struct {
		setup func(string, string)
		cause string
	}{
		"not mounted": {func(dir, _ string) {
			if err := os.RemoveAll(filepath.Join(dir, ".agent", "runs")); err != nil {
				t.Fatal(err)
			}
		}, ".agent/runs is not a folder in this box"},
		"missing": {func(_, _ string) {}, ".agent/runs/unsafe.peers.jsonl is not a file in this box"},
		"permissive mode": {func(_, path string) {
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}, ".agent/runs/unsafe.peers.jsonl is not a private file"},
		"hardlink": {func(_, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(path, path+".other"); err != nil {
				t.Fatal(err)
			}
		}, ".agent/runs/unsafe.peers.jsonl is not a private file"},
		"symlink": {func(dir, path string) {
			target := filepath.Join(dir, "outside")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}, ".agent/runs/unsafe.peers.jsonl is not a file in this box"},
	}
	for name, tc := range unsafe {
		t.Run(name, func(t *testing.T) {
			// Three consultations in one box: the reason is said once, never once per call.
			row, out := run("unsafe", 3, tc.setup)
			if row != "" {
				t.Errorf("unsafe peer telemetry target was appended: %q", row)
			}
			if want := "[codex: usage not recorded: " + tc.cause + "]\n"; out != want {
				t.Errorf("output = %q, want the one notice %q", out, want)
			}
		})
	}

	t.Run("pathname swap after validation", func(t *testing.T) {
		dir := t.TempDir()
		runs := filepath.Join(dir, ".agent", "runs")
		if err := os.MkdirAll(runs, 0o755); err != nil {
			t.Fatal(err)
		}
		peerFile := filepath.Join(runs, "swap.peers.jsonl")
		outside := filepath.Join(dir, "outside")
		if err := os.WriteFile(peerFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, []byte("DO_NOT_APPEND\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		realStat, err := exec.LookPath("stat")
		if err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(dir, "bin")
		if err := os.Mkdir(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		// Swap immediately after the successful pre-open permission check. The shell then opens the
		// symlink, but its descriptor/path identity recheck must reject it before appending. Like the
		// real stat, a failed call prints nothing: the shell falls back from GNU to BSD flags.
		statShim := `#!/bin/sh
out=$($REAL_STAT "$@")
status=$?
if [ "$status" -eq 0 ]; then
  case " $* " in
  *" %a "* | *" %Lp "*)
    if [ ! -e "$SWAPPED" ]; then
      : >"$SWAPPED"
      rm -f "$PEER_FILE"
      ln -s "$OUTSIDE" "$PEER_FILE"
    fi
    ;;
  esac
  printf '%s\n' "$out"
fi
exit "$status"
`
		if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(statShim), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", script, "sh", stream)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"), "COOP_RUN_ID=swap", "REAL_STAT="+realStat,
			"PEER_FILE="+peerFile, "OUTSIDE="+outside, "SWAPPED="+filepath.Join(dir, "swapped"), "TMPDIR="+t.TempDir(),
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("shell failed: %v\n%s", err, out)
		}
		got, err := os.ReadFile(outside)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "DO_NOT_APPEND\n" {
			t.Fatalf("path swap redirected telemetry into another file: %q", got)
		}
		if want := "[codex: usage not recorded: .agent/runs/swap.peers.jsonl changed while it was opened]\n"; string(out) != want {
			t.Fatalf("output = %q, want %q", out, want)
		}
	})
}

func TestRoleHealthShellUsesRepositoryRoot(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("init fixture repo: %v\n%s", err, out)
	}
	runs := filepath.Join(repo, ".agent", "runs")
	if err := os.MkdirAll(runs, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(runs, "nested.peers.jsonl")
	if err := os.WriteFile(record, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, "portal")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	script := RoleHealthShell() + `
coop_role_health thinker consult codex model codex:model failed 1 true unavailable
coop_role_quarantined codex:model
`
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = nested
	cmd.Env = append(os.Environ(), "COOP_RUN_ID=nested")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("role health shell failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind":"role_health"`, `"target":"codex:model"`, `"permanent":true`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("role health row missing %q: %s", want, data)
		}
	}
}

func TestRoleHealthShellEmptyFileIsNotQuarantined(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("init fixture repo: %v\n%s", err, out)
	}
	runs := filepath.Join(repo, ".agent", "runs")
	if err := os.MkdirAll(runs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runs, "empty.peers.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(repo, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// jq 1.6 exits zero for an empty input stream with `-e`. Model that behavior so
	// quarantine depends on an explicit false result, not jq's version-specific status.
	if err := os.WriteFile(filepath.Join(bin, "jq"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := RoleHealthShell() + `
if coop_role_quarantined codex:model; then exit 42; fi
`
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "COOP_RUN_ID=empty")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("empty peer file was treated as quarantined: %v\n%s", err, out)
	}
}
