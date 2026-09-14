package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCodexModelsMenu(t *testing.T) {
	want := []string{
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.3-codex-spark",
	}
	if got := (codexAgent{}).Models(); !slices.Equal(got, want) {
		t.Fatalf("codex Models() = %v, want current list-visible catalog %v", got, want)
	}
}

// TestCodexPeerRowShell runs the ACTUAL generated codex_peer_row (from ShellPrelude) against a
// real-shaped `codex exec --json` turn.completed event and asserts it appends one peer-usage row:
// input_tokens as "in", output_tokens+reasoning_output_tokens as "out". With no COOP_RUN_ID (not a
// loop) it must write nothing. Skipped where jq is absent (the box always has it).
func TestCodexPeerRowShell(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	// The turn.completed shape verified against a live `codex exec --json` run (2026-07-13).
	stream := `{"type":"turn.completed","usage":{"input_tokens":15880,"cached_input_tokens":9984,"output_tokens":5,"reasoning_output_tokens":0}}`
	script := codexAgent{}.ShellPrelude() + "\nprintf '%s\\n' \"$1\" | codex_peer_row thinker gpt-5.6-terra\n"

	run := func(runID string, setup ...func(string, string)) string {
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
		cmd := exec.Command("sh", "-c", script, "sh", stream)
		cmd.Dir = nested
		cmd.Env = os.Environ()
		if runID != "" {
			cmd.Env = append(cmd.Env, "COOP_RUN_ID="+runID)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("shell failed: %v\n%s", err, out)
		}
		b, _ := os.ReadFile(filepath.Join(dir, ".agent", "runs", runID+".peers.jsonl"))
		return strings.TrimSpace(string(b))
	}

	row := run("r1")
	for _, want := range []string{`"run":"r1"`, `"role":"thinker"`, `"provider":"codex"`, `"model":"gpt-5.6-terra"`, `"in":15880`, `"out":5`} {
		if !strings.Contains(row, want) {
			t.Errorf("peer row missing %q: %q", want, row)
		}
	}
	if row := run(""); row != "" {
		t.Errorf("no COOP_RUN_ID must write no row, got %q", row)
	}
	unsafe := map[string]func(string, string){
		"missing": func(_, _ string) {},
		"permissive mode": func(_, path string) {
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink": func(_, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(path, path+".other"); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(dir, path string) {
			target := filepath.Join(dir, "outside")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range unsafe {
		t.Run(name, func(t *testing.T) {
			if row := run("unsafe", setup); row != "" {
				t.Errorf("unsafe peer telemetry target was appended: %q", row)
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
		// symlink, but its descriptor/path identity recheck must reject it before appending.
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
fi
printf '%s\n' "$out"
exit "$status"
`
		if err := os.WriteFile(filepath.Join(bin, "stat"), []byte(statShim), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", script, "sh", stream)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"), "COOP_RUN_ID=swap", "REAL_STAT="+realStat,
			"PEER_FILE="+peerFile, "OUTSIDE="+outside, "SWAPPED="+filepath.Join(dir, "swapped"),
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("shell failed: %v\n%s", err, out)
		}
		got, err := os.ReadFile(outside)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "DO_NOT_APPEND\n" {
			t.Fatalf("path swap redirected telemetry into another file: %q", got)
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
