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

func TestEvalCompletionFollowsTheWorkflow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := seedEvalResults(t, "20260921-results", true)
	if err := os.Mkdir(filepath.Join(filepath.Dir(dir), "starters"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := freshConfig(t)
	if err := os.MkdirAll(cfg.AgentProfileDir("codex", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}
	for _, words := range [][]string{{"eval", "inspect"}, {"eval", "compare"}, {"eval", "compare", "before"}} {
		got := a.completionCandidates(words)
		if !hasCand(got, "20260921-results") || hasCand(got, "starters") {
			t.Errorf("%v candidates = %v", words, got)
		}
	}
	for _, s := range []string{"core", "queue"} {
		if !hasCand(a.completionCandidates([]string{"eval", "run"}), s) {
			t.Errorf("suite missing: %s", s)
		}
	}
	for _, words := range [][]string{{"eval", "run", "core", "--timeout"}, {"eval", "run", "core", "--loop-config"}} {
		if got := a.completionCandidates(words); len(got) != 0 {
			t.Errorf("models offered as a flag value: %v", got)
		}
	}
	got := a.completionCandidates([]string{"eval", "run", "core", "--timeout", "35m"})
	if !hasCand(got, "codex") || !hasCand(got, "--dry-run") || hasCand(got, "--timeout") {
		t.Errorf("after interleaved flag: %v", got)
	}
	if got := a.completionCandidatesFor([]string{"eval", "run", "core"}, "codex@"); !hasCand(got, "codex@work") {
		t.Errorf("account target missing: %v", got)
	}
}

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
	for _, w := range []string{"fork", "tasks", "loop", "approve", "claude", "grok", "completion"} {
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
	if hasCand(a.completionCandidates([]string{"net"}), "approve") {
		t.Error("retired `coop net approve` must not be completed")
	}

	for _, w := range []string{"ls", "rm", "merge"} {
		if !hasCand(a.completionCandidates([]string{"fork"}), w) {
			t.Errorf("fork completion missing verb %q", w)
		}
	}
	if tk := a.completionCandidates([]string{"tasks"}); !hasCand(tk, "claim") || !hasCand(tk, "watch") {
		t.Errorf("tasks completion missing verbs: %v", tk)
	}
	if !hasCand(a.completionCandidates([]string{"login"}), "claude") {
		t.Error("login completion should offer agents")
	}
	if c := a.completionCandidates([]string{"completion"}); !hasCand(c, "bash") || !hasCand(c, "zsh") {
		t.Errorf("completion completion missing shells: %v", c)
	}
	if c := a.completionCandidates([]string{"sessions"}); !hasCand(c, "compact") {
		t.Errorf("sessions completion missing compact: %v", c)
	}
	if c := a.completionCandidates([]string{"sessions", "compact"}); !hasCand(c, "--state") || !hasCand(c, "--backup") {
		t.Errorf("sessions compact completion missing flags: %v", c)
	}

	loop := a.completionCandidatesFor([]string{"loop"}, "")
	for _, want := range []string{"claude", "claude:opus", "codex:gpt-5.5", "--peer", "--review-task", "--max-tasks", "--no-mcp"} {
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
	// Gemini thinks at low or high only; completion offers exactly what the target would accept.
	if got := a.completionCandidatesFor([]string{"loop"}, "gemini:gemini-3.8-flash/"); !hasCand(got, "gemini:gemini-3.8-flash/high") ||
		!hasCand(got, "gemini:gemini-3.8-flash/low") || hasCand(got, "gemini:gemini-3.8-flash/medium") || hasCand(got, "gemini/xhigh") {
		t.Errorf("gemini completion must offer exactly its expressible effort levels: %v", got)
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
	for _, reserved := range []string{"ls", "acp"} {
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

func TestCompletionKeepsTheCurrentWord(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}}

	for _, tc := range []struct {
		words []string
		want  string
	}{
		{[]string{"loop"}, "loop\n"},
		{[]string{"loop", ""}, "claude\n"},
	} {
		out := captureCompletionOutput(t, func() {
			if code, err := a.cmdComplete(tc.words); code != 0 || err != nil {
				t.Fatalf("cmdComplete(%q) = (%d, %v)", tc.words, code, err)
			}
		})
		if !strings.Contains(out, tc.want) || (len(tc.words) == 1 && out != tc.want) {
			t.Errorf("cmdComplete(%q) must complete the current word, want %q:\n%s", tc.words, tc.want, out)
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

func TestBashCompletionPreservesWordsAndCandidates(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	for _, cur := range []string{"", "cod"} {
		t.Run("current="+cur, func(t *testing.T) {
			// The backend rejects a merged "loop " argument and returns candidates whose spaces
			// and glob syntax must survive literally. macOS /bin/bash exercises Bash 3.2 here.
			script := `coop() {
  [ "$#" -eq 3 ] && [ "$1" = __complete ] && [ "$2" = loop ] && [ "$3" = "$current" ] || return 97
  printf '%s\n' 'codex' 'two words' '*'
}
`
			// Pass the current token as an argument, not interpolated shell source.
			script = "current=$1\n" + script + bashCompletion + `
COMP_WORDS=(coop loop "$current")
COMP_CWORD=2
_coop
printf '<%s>\n' "${COMPREPLY[@]}"
`
			cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script, "completion-test", cur)
			cmd.Dir = t.TempDir()
			if err := os.WriteFile(filepath.Join(cmd.Dir, "would-expand"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + cmd.Dir}
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != "<codex>\n<two words>\n<*>\n" {
				t.Fatalf("generated Bash completion lost words/candidates: %v\n%s", err, out)
			}
		})
	}
}

func TestBashCompletionModelWordBreaks(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	for _, tc := range []struct {
		name, query, breaks, want string
		words                     []string
	}{
		{"joined partial", "codex:q", ":", "qualified", []string{"coop", "loop", "codex:q"}},
		{"split partial", "codex:q", ":", "qualified", []string{"coop", "loop", "codex", ":", "q"}},
		{"joined empty", "codex:", ":", "qualified", []string{"coop", "loop", "codex:"}},
		{"split empty", "codex:", ":", "qualified", []string{"coop", "loop", "codex", ":"}},
		{"custom word breaks", "codex:q", "", "codex:qualified", []string{"coop", "loop", "codex:q"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := bashCompletion + `
query=$1
COMP_WORDBREAKS=$2
shift 2
COMP_WORDS=("$@")
COMP_CWORD=$((${#COMP_WORDS[@]}-1))
coop() {
  [ "$#" -eq 3 ] && [ "$1" = __complete ] && [ "$2" = loop ] && [ "$3" = "$query" ] || return 97
  printf '%s\n' 'codex:qualified'
}
_coop
printf '<%s>\n' "${COMPREPLY[@]}"
`
			args := append([]string{"--noprofile", "--norc", "-c", script, "completion-test", tc.query, tc.breaks}, tc.words...)
			cmd := exec.Command(bash, args...)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != "<"+tc.want+">\n" {
				t.Fatalf("generated Bash model completion: %v\n%s", err, out)
			}
		})
	}
}

func TestZshCompletionPreservesEmptyWord(t *testing.T) {
	if !strings.Contains(zshCompletion, `coop __complete "${(@)words[2,$CURRENT]}"`) {
		t.Fatalf("zsh completion must quote the word array expansion:\n%s", zshCompletion)
	}
	// The generated comments name the SAME user-controlled path the help page does, so following
	// either one lands in the same place; the executable body is unchanged.
	for _, want := range []string{
		"# Source this file after compinit in ~/.zshrc.",
		"coop completion zsh > ~/.config/coop/completion.zsh",
		"compdef _coop coop", "alias coop='nocorrect coop'",
	} {
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
	// The real collision: a Coop project holds a directory named after every agent it set up, so
	// CORRECT_ALL reads `coop claude` as a misspelled `.claude`.
	for _, dir := range []string{".claude", ".codex", ".gemini"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
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
foreach agent {claude codex gemini} {
  send -- "coop $agent\r"
  expect {
    -re "correct '$agent' to '\\.$agent'" { send -- "n\r" }
    timeout { puts stderr "baseline did not reproduce Zsh correction for $agent"; exit 10 }
  }
  expect -re "STUB:$agent"
}
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

# Every agent argument now reaches the shim with no correction prompt in the way.
foreach agent {claude codex gemini} {
  send -- "coop $agent\r"
  expect {
    -re "correct '$agent' to '\\.$agent'" { puts stderr "coop $agent was still corrected"; exit 14 }
    -re "STUB:$agent" {}
    timeout { puts stderr "coop $agent did not reach the stub"; exit 15 }
  }
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

// Completion offers each sessions subcommand's flags exactly as its parser accepts them, and nothing
// for a subcommand that doesn't exist (serve and policies once lingered here after they were gone).
func TestSessionsCompletionMatchesTheParsers(t *testing.T) {
	a := &app{}
	parse := map[string]func([]string) error{
		"connect": func(args []string) error { _, err := parseSessionConnectFlags(args); return err },
		"doctor":  func(args []string) error { _, _, err := parseSessionDoctorFlags(args); return err },
		"compact": func(args []string) error { _, _, err := parseSessionCompactFlags(args); return err },
	}
	unknown := func(err error) bool { return err != nil && strings.Contains(strings.ToLower(err.Error()), "unknown") }
	for _, sub := range sessionCommands {
		check, ok := parse[sub]
		if !ok {
			t.Fatalf("no parser registered for sessions %s", sub)
		}
		if !unknown(check([]string{"--bogus"})) {
			t.Fatalf("sessions %s accepted --bogus, so this test cannot tell a stale flag", sub)
		}
		flags := a.completionCandidates([]string{"sessions", sub})
		if len(flags) == 0 {
			t.Errorf("sessions %s completes no flags", sub)
		}
		for _, flag := range flags {
			args := []string{flag}
			if flag != "--json" {
				args = append(args, "/tmp/value")
			}
			if err := check(args); unknown(err) {
				t.Errorf("completion offers sessions %s %s, which its parser rejects: %v", sub, flag, err)
			}
		}
	}
	for _, gone := range []string{"serve", "policies"} {
		if c := a.completionCandidates([]string{"sessions", gone}); len(c) != 0 {
			t.Errorf("completion offers flags for sessions %s, which doesn't exist: %v", gone, c)
		}
	}
}
