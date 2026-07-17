package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func hasCand(cands []string, want string) bool {
	return candCount(cands, want) > 0
}

func candCount(cands []string, want string) int {
	n := 0
	for _, c := range cands {
		if c == want {
			n++
		}
	}
	return n
}

// completionCandidates mirrors the dispatch: commands + agents at the top, then per-family verbs.
func TestCompletionCandidates(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}}

	top := a.completionCandidates(nil)
	for _, w := range []string{"fork", "tasks", "loop", "claude", "grok", "completion"} {
		if !hasCand(top, w) {
			t.Errorf("top-level completion missing %q", w)
		}
	}
	if hasCand(top, "clone") || hasCand(top, "pool") {
		t.Error("retired aliases (clone/pool) must not be completed")
	}
	// completion appears exactly once (topLevelCommands already carries it — no separate prepend).
	if n := candCount(top, "completion"); n != 1 {
		t.Errorf("completion should be offered exactly once, got %d", n)
	}
	// The retired-form invariant must hold per-scope, not just top-level: `coop loop <TAB>` offered
	// the tombstoned `pool` before this fix, and only the top-level list was ever checked.
	if hasCand(a.completionCandidates([]string{"loop"}), "pool") {
		t.Error("`coop loop` must not complete the retired `pool` (it's tombstoned)")
	}

	for _, w := range []string{"ls", "rm", "merge"} {
		if !hasCand(a.completionCandidates([]string{"fork"}), w) {
			t.Errorf("fork completion missing verb %q", w)
		}
	}
	if tk := a.completionCandidates([]string{"tasks"}); !hasCand(tk, "claim") || !hasCand(tk, "watch") {
		t.Errorf("tasks completion missing verbs: %v", tk)
	}
	if fl := a.completionCandidates([]string{"fleet"}); !hasCand(fl, "up") || !hasCand(fl, "prune") {
		t.Errorf("fleet completion missing verbs: %v", fl)
	}
	if !hasCand(a.completionCandidates([]string{"login"}), "claude") {
		t.Error("login completion should offer agents")
	}
	if c := a.completionCandidates([]string{"completion"}); !hasCand(c, "bash") || !hasCand(c, "zsh") {
		t.Errorf("completion completion missing shells: %v", c)
	}

	loop := a.completionCandidatesFor([]string{"loop"}, "")
	for _, want := range []string{"claude", "claude:opus", "codex:gpt-5.5", "--peer", "--max-tasks", "--no-mcp"} {
		if !hasCand(loop, want) {
			t.Errorf("loop completion missing %q: %v", want, loop)
		}
	}
	if hasCand(loop, "pool") {
		t.Error("loop completion must not offer the retired pool command")
	}
	if got := a.completionCandidatesFor([]string{"loop", "--max-tasks"}, ""); !hasCand(got, "1") || !hasCand(got, "3") {
		t.Errorf("max-tasks completion missing numeric values: %v", got)
	}

	if got := a.completionCandidatesFor(nil, "claude:"); !hasCand(got, "claude:opus") {
		t.Errorf("model-prefix completion missing claude:opus: %v", got)
	}
	if got := a.completionCandidatesFor([]string{"loop"}, "codex:gpt-5.5/"); !hasCand(got, "codex:gpt-5.5/high") {
		t.Errorf("effort-prefix completion missing codex:gpt-5.5/high: %v", got)
	}
	if got := a.completionCandidatesFor([]string{"loop"}, "grok:grok-4.5/"); !hasCand(got, "grok:grok-4.5/high") {
		t.Errorf("effort-prefix completion missing grok:grok-4.5/high: %v", got)
	}
	if got := a.completionCandidatesFor([]string{"loop"}, "gemini:gemini-3.5-flash/"); hasCand(got, "gemini:gemini-3.5-flash/high") {
		t.Errorf("gemini completion must not offer unsupported effort levels: %v", got)
	}

	for _, prev := range [][]string{{"fork", "work"}, {"fork", "work", "acp"}} {
		got := a.completionCandidatesFor(prev, "grok:")
		if !hasCand(got, "grok:grok-4.5") {
			t.Errorf("%q target completion missing grok:grok-4.5: %v", prev, got)
		}
	}
	if got := a.completionCandidatesFor([]string{"fork", "work"}, ""); !hasCand(got, "acp") {
		t.Errorf("fork target completion must offer ACP mode: %v", got)
	}
	if got := a.completionCandidatesFor([]string{"fork", "work", "acp"}, ""); !hasCand(got, "--peer") {
		t.Errorf("fork ACP target completion must offer peers: %v", got)
	}
	if got := a.completionCandidatesFor([]string{"fork", "work", "acp", "claude"}, ""); !hasCand(got, "--peer") {
		t.Errorf("fork ACP provider completion must offer peers: %v", got)
	}
	if got := a.completionCandidatesFor([]string{"fork", "work", "acp", "codex", "--peer"}, "grok:"); !hasCand(got, "grok:grok-4.5") {
		t.Errorf("fork ACP peer completion missing Grok target: %v", got)
	}
	for _, reserved := range []string{"ls", "acp", "watch"} {
		if got := a.completionCandidatesFor([]string{"fork", reserved}, ""); len(got) != 0 {
			t.Errorf("fork reserved word %q offered invalid trailing candidates: %v", reserved, got)
		}
	}
}

func TestCompletionTargetsAccountsAndPresets(t *testing.T) {
	repo, cfg := t.TempDir(), &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir()}
	cfg.RepoOverride = repo
	if err := os.MkdirAll(cfg.AgentProfileDir("claude", "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	presetDir := filepath.Join(repo, ".agent", "presets", "frontier")
	if err := os.MkdirAll(presetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetDir, "preset.yaml"), []byte("broken: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &app{cfg: cfg}
	got := a.completionCandidatesFor([]string{"loop"}, "claude:opus@")
	for _, want := range []string{"claude:opus@work", "frontier"} {
		if !hasCand(got, want) {
			t.Errorf("target completion missing %q: %v", want, got)
		}
	}
	if got := a.completionCandidatesFor([]string{"loop", "--peer"}, "claude:opus@"); hasCand(got, "claude:opus@work") {
		t.Errorf("peer completion must not offer account-pinned targets: %v", got)
	}
	if got := a.completionCandidatesFor([]string{"loop", "claude", "--peer"}, ""); !hasCand(got, "codex:gpt-5.5") {
		t.Errorf("peer completion after the loop target missing codex:gpt-5.5: %v", got)
	}
	if got := a.completionCandidatesFor([]string{"fork", "work"}, ""); !hasCand(got, "frontier") {
		t.Errorf("fork target completion missing preset: %v", got)
	}
}

func TestCompletionAdvancesExactCommand(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}}

	for _, words := range [][]string{{"loop"}, {"loop", ""}} {
		out := captureCompletionOutput(t, func() {
			if code, err := a.cmdComplete(words); code != 0 || err != nil {
				t.Fatalf("cmdComplete(%q) = (%d, %v)", words, code, err)
			}
		})
		if !strings.Contains(out, "claude\n") || strings.Contains(out, "\nloop\n") {
			t.Errorf("cmdComplete(%q) did not advance to target candidates:\n%s", words, out)
		}
	}

	out := captureCompletionOutput(t, func() {
		if code, err := a.cmdComplete([]string{"lo"}); code != 0 || err != nil {
			t.Fatalf("cmdComplete(lo) = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(out, "loop\n") {
		t.Errorf("partial top-level command did not complete loop:\n%s", out)
	}
}

func TestZshCompletionPreservesEmptyWord(t *testing.T) {
	if !strings.Contains(zshCompletion, `coop __complete "${(@)words[2,$CURRENT]}"`) {
		t.Fatalf("zsh completion must quote the word array expansion:\n%s", zshCompletion)
	}
	for _, want := range []string{"source that file AFTER", "compdef _coop coop", "alias coop='nocorrect coop'"} {
		if !strings.Contains(zshCompletion, want) {
			t.Fatalf("zsh integration missing %q:\n%s", want, zshCompletion)
		}
	}
}

func TestZshCompletionSuppressesOnlyCoopCorrection(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not available")
	}
	expect, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect not available for interactive PTY regression")
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	coopStub := `#!/bin/sh
if [ "${1:-}" = __complete ]; then
  printf 'codex\n'
  exit 0
fi
printf 'STUB:%s\n' "$*"
`
	otherStub := "#!/bin/sh\nprintf 'OTHER:%s\\n' \"$*\"\n"
	for name, body := range map[string]string{"coop": coopStub, "other": otherStub} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	completion := filepath.Join(root, "_coop")
	if err := os.WriteFile(completion, []byte(zshCompletion), 0o644); err != nil {
		t.Fatal(err)
	}

	expectScript := `set timeout 8
set root [lindex $argv 0]
set zsh [lindex $argv 1]
set completion [lindex $argv 2]
set env(PATH) "$root/bin:$env(PATH)"
set env(HOME) $root
cd $root

proc start_shell {zsh setup} {
  global spawn_id
  spawn $zsh -f -i
  send -- "$setup\r"
}

# Prove the fixture reproduces the reported correction before installing Coop's integration.
start_shell $zsh "setopt correct_all"
send -- "coop loop codex\r"
expect {
  -re {correct 'codex' to '.codex'} { send -- "n\r" }
  timeout { puts stderr "baseline did not reproduce Zsh correction"; exit 10 }
}
expect -re {STUB:loop codex}
send -- "exit\r"
expect eof

# Source the generated integration after compinit, exercise dynamic completion, and execute.
start_shell $zsh "setopt correct_all; autoload -Uz compinit; compinit -u -d $root/.zcompdump; source $completion"
send -- "coop loop cod\t"
expect -re {coop loop codex}
send -- "\r"
expect {
  -re {correct 'codex' to '.codex'} { puts stderr "coop argument correction was not suppressed"; exit 11 }
  -re {STUB:loop codex} {}
  timeout { puts stderr "completed coop command did not reach the stub"; exit 12 }
}

# The alias is command-local: correction remains active for another command.
send -- "other codex\r"
expect {
  -re {correct 'codex' to '.codex'} { send -- "n\r" }
  timeout { puts stderr "coop integration disabled correction globally"; exit 13 }
}
expect -re {OTHER:codex}
send -- "exit\r"
expect eof
`
	script := filepath.Join(root, "correction.exp")
	if err := os.WriteFile(script, []byte(expectScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(expect, script, root, zsh, completion)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("interactive Zsh correction regression: %v\n%s", err, out)
	}
}

func captureCompletionOutput(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// `coop tasks claim <TAB>` offers the queue's task ids (a local read).
func TestCompletionTaskIDs(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-wire-auth", "task.md"), "# x\n")
	a := &app{cfg: &config.Config{RepoOverride: repo, TasksFiles: []string{tasksRoot}}}
	if ids := a.completionCandidates([]string{"tasks", "claim"}); !hasCand(ids, "2026-01-01-wire-auth") {
		t.Errorf("tasks claim completion should offer task ids, got %v", ids)
	}
	// a non-id verb does not offer ids.
	if ids := a.completionCandidates([]string{"tasks", "lint"}); hasCand(ids, "2026-01-01-wire-auth") {
		t.Error("tasks lint takes no id — should not offer task ids")
	}
}
