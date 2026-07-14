package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/ui"
)

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// fenceMarker reports whether a line opens or closes a Markdown fenced code block (``` or ~~~ —
// three or more, ignoring leading whitespace and any info string). Task-body scanners toggle on
// it so a "- [ ]" documented INSIDE a fence (e.g. an example in a task body) isn't read as a real
// subtask.
func fenceMarker(line string) bool {
	t := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// taskCounts tallies a task queue by state (todo/in_progress/blocked/done).
type taskCounts struct{ Todo, Doing, Done, Blocked int }

func (c taskCounts) total() int { return c.Todo + c.Doing + c.Done + c.Blocked }

type queuedTask struct {
	Root string
	Item taskItem
}

// queueState reads the queue union once, tallying it and selecting the authoritative next task.
// An interrupted task wins globally, even when an earlier subproject queue still has todo work;
// ties preserve queue order and readTaskTree's stable ID order.
func queueState(hosts []string) (taskCounts, queuedTask, bool) {
	var total taskCounts
	var firstTodo, firstDoing queuedTask
	haveTodo, haveDoing := false, false
	for _, h := range hosts {
		for _, t := range readTaskTree(h) {
			switch t.State {
			case stateTodo:
				total.Todo++
				if !haveTodo {
					firstTodo, haveTodo = queuedTask{Root: h, Item: t}, true
				}
			case stateInProgress:
				total.Doing++
				if !haveDoing {
					firstDoing, haveDoing = queuedTask{Root: h, Item: t}, true
				}
			case stateBlocked:
				total.Blocked++
			case stateDone:
				total.Done++
			}
		}
	}
	if haveDoing {
		return total, firstDoing, true
	}
	return total, firstTodo, haveTodo
}

// queueProgress sums task counts across the queue(s) and returns the authoritative next task's
// title, sharing queueState with the loop assignment so its banner cannot disagree with the box.
func queueProgress(hosts []string) (taskCounts, string) {
	total, next, ok := queueState(hosts)
	if !ok {
		return total, ""
	}
	return total, next.Item.Title
}

// progressLine is the queue's at-a-glance state: done/total (done greened when nonzero), a
// blocked tally only when there is one, and the task being worked. The loop prints it both
// in the per-iteration banner and live, on its own, whenever a task changes state mid-run.
func progressLine(c taskCounts, active string) string {
	s := fmt.Sprintf("%s/%d done", paintCount(c.Done, ui.Green), c.total())
	if c.Blocked > 0 {
		s += fmt.Sprintf(" · %s blocked", paintCount(c.Blocked, ui.Red))
	}
	if active != "" {
		s += " · now: " + truncate(active, 48)
	}
	return s
}

// progressBanner is progressLine prefixed with the iteration number, printed at the top of
// each loop iteration.
func progressBanner(n int, c taskCounts, active string) string {
	return fmt.Sprintf("iteration %d · %s", n, progressLine(c, active))
}

// paintCount renders a count, applying paint only when it's nonzero so a zero stays
// plain — a "0 blocked" shouldn't read as an alarm. Shared by the `coop tasks` summary
// and the loop banner.
func paintCount(v int, paint func(string) string) string {
	if v > 0 {
		return paint(strconv.Itoa(v))
	}
	return strconv.Itoa(v)
}

// readFileString returns a file's contents, or "" if it can't be read.
func readFileString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// truncate shortens s to n runes, marking elision with an ellipsis.
func truncate(s string, n int) string {
	if n <= 0 {
		return "" // guards the r[:n-1] / r[:n] negative-index panic on a non-positive width
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// wrapWords greedily word-wraps s into lines no wider than w runes, breaking on whitespace; a
// single word longer than w is hard-split so a line can never overflow. Always returns at least
// one line (possibly ""). Like truncate it counts runes, not display cells — fine for the common
// ASCII title; a wide-script title may wrap a column early.
func wrapWords(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	cur, curLen := "", 0
	for _, word := range words {
		wr := []rune(word)
		for len(wr) > w { // hard-split a word longer than the whole budget
			if curLen > 0 {
				lines = append(lines, cur)
				cur, curLen = "", 0
			}
			lines = append(lines, string(wr[:w]))
			wr = wr[w:]
		}
		word = string(wr)
		switch {
		case curLen == 0:
			cur, curLen = word, len(wr)
		case curLen+1+len(wr) <= w:
			cur, curLen = cur+" "+word, curLen+1+len(wr)
		default:
			lines = append(lines, cur)
			cur, curLen = word, len(wr)
		}
	}
	if curLen > 0 || len(lines) == 0 {
		lines = append(lines, cur)
	}
	return lines
}

// sanitizeCell strips control characters (notably ESC, 0x1b) from a one-line display string so a
// task title or decision text — which can come from an untrusted agent's task.md — can't inject
// ANSI escapes into coop's output: corrupting a redirect/pipe, or spoofing the colored UI on a
// terminal. Single-line cells carry no legitimate control chars, so all of them are dropped.
func sanitizeCell(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// levenshtein returns the edit distance between a and b, for "did you mean" suggestions.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// nearestCommand suggests the candidate closest to a mistyped command. The allowed edit distance
// scales with input length so short words don't attract noise: 1-2 runes get no suggestion (fuzzy
// matches on `ls`/`go`/`cp` are mostly noise, and the caller's "run it in the box" hint covers them),
// 3 runes match only at distance 1 (`lop`→loop, `lss`→ls — a single slip of the most-typed verbs),
// and 4+ runes match within 2. This is what catches a distance-1 typo of `ls` before `coop fork lss`
// silently clones a stray fork.
func nearestCommand(input string, candidates []string) (string, bool) {
	n := len([]rune(input))
	if n < 3 {
		return "", false
	}
	maxDist := 2
	if n == 3 {
		maxDist = 1
	}
	best, bestDist := "", -1
	for _, c := range candidates {
		if d := levenshtein(input, c); bestDist < 0 || d < bestDist {
			best, bestDist = c, d
		}
	}
	if bestDist >= 0 && bestDist <= maxDist {
		return best, true
	}
	return "", false
}

// rejectArgs returns a usage error when a command that takes no arguments is given some,
// so a stray token fails clearly instead of being silently ignored. (A `help`/`--help`
// arg is intercepted earlier, so it never reaches here.) No leading "coop " — ui.Error already
// prefixes "coop:", so this would otherwise double it ("coop: coop doctor …").
func rejectArgs(cmd string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("%s takes no arguments (got %q) — see 'coop %s --help'", cmd, strings.Join(args, " "), cmd)
}

// colWidth is the width to size a table column to: the widest value (counted in runes), clamped
// to [min, max]. Values longer than max are meant to be ellipsis-truncated to max by the caller.
func colWidth(values []string, min, max int) int {
	w := min
	for _, v := range values {
		if n := utf8.RuneCountInString(v); n > w {
			w = n
		}
	}
	if w > max {
		w = max
	}
	return w
}

// padRight right-pads s to w columns counted in RUNES — unlike fmt's %-Ns, which counts bytes and
// so mis-pads a value carrying a multibyte glyph (e.g. a truncated name's "…").
func padRight(s string, w int) string {
	if n := utf8.RuneCountInString(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// unknownErr is the one shape for a rejected subcommand / agent / value: `unknown <noun>
// "<token>" — use: a, b, c`, with a "did you mean X?" when the token is a near-miss. Shared by the
// sub-command groups (tasks/fleet/pool/profiles) so a bad input reads the same everywhere.
func unknownErr(noun, token string, valid []string) error {
	if guess, ok := nearestCommand(token, valid); ok {
		return fmt.Errorf("unknown %s %q — use: %s (did you mean %q?)", noun, token, strings.Join(valid, ", "), guess)
	}
	return fmt.Errorf("unknown %s %q — use: %s", noun, token, strings.Join(valid, ", "))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// gitClone clones src→dst carrying gitHardening like every other repo-touching git call:
// inert for a plain local clone, but defense in depth if the source's config was poisoned.
func gitClone(src, dst string) error {
	args := append(append([]string{}, gitHardening...), "clone", "--quiet", src, dst)
	return exec.Command("git", args...).Run()
}

func gitCheckoutNewBranch(repo, branch string) error {
	return exec.Command("git", "-C", repo, "checkout", "--quiet", "-b", branch).Run()
}

// gitArgs builds `git -C dir <hardening> <args>`. The hardening goes first so a caller's own
// trailing -c flags (e.g. trustedSignArgs) still win — git's last -c for a key takes effect.
func gitArgs(dir string, args []string) []string {
	return append(append([]string{"-C", dir}, gitHardening...), args...)
}

// gitOut runs `git -C dir <args>` hardened and returns trimmed stdout, or "" on error. Every repo
// coop runs git against is agent-writable, so hardening is the default; to read a value coop will
// execute or read a host file from, use gitGlobalOut (the trusted global scope), never the repo.
func gitOut(dir string, args ...string) string {
	out, err := exec.Command("git", gitArgs(dir, args)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRun runs `git -C dir <args>` hardened, for effect, returning its error.
func gitRun(dir string, args ...string) error {
	return exec.Command("git", gitArgs(dir, args)...).Run()
}

// gitInteractive runs a hardened git command wired to the real stdio (a diff to the terminal, a
// signing pinentry prompt, etc).
func gitInteractive(dir string, args ...string) error {
	cmd := exec.Command("git", gitArgs(dir, args)...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

// gitSign runs a hardened git command (like a rebase with signing), wiring Stdin
// so a TTY pinentry still works, but capturing CombinedOutput to silence benign chatter.
// The captured output is replayed to Stderr only on failure, or if GIT_TRACE is set.
func gitSign(dir string, args ...string) error {
	return gitSignTo(os.Stderr, dir, args...)
}

func gitSignTo(stderr io.Writer, dir string, args ...string) error {
	cmd := exec.Command("git", gitArgs(dir, args)...)
	cmd.Stdin = os.Stdin
	out, err := cmd.CombinedOutput()
	trace := strings.TrimSpace(os.Getenv("GIT_TRACE"))
	if err != nil || (trace != "" && trace != "0" && !strings.EqualFold(trace, "false")) {
		_, _ = stderr.Write(out)
	}
	return err
}

// gitGlobalOut reads from the host user's GLOBAL git config (`git config --global …`) — the
// trusted scope an agent can't write — for any value coop reads then EXECUTES or reads a host file
// from: your core.editor, your signing program, your global core.excludesfile. The repo's own
// .git/config is agent-writable, so reading these from it would let a poisoned repo redirect coop
// to run or exfiltrate whatever it names. They're all user-identity settings that live in your
// global config anyway; a value only in repo config is treated as unset (fail closed). Returns ""
// when unset or git is unavailable.
func gitGlobalOut(args ...string) string {
	out, err := exec.Command("git", append([]string{"config", "--global"}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitHardening are -c overrides applied to EVERY git command coop runs for effect on a working
// tree, because every repo coop touches is agent-writable: the box binds the repo (its .git
// included) read-write on a normal run, so an agent can plant hooks or local config that execute
// host commands the moment coop fetches, rebases, merges, diffs, or even `status`es it — whether
// that's a fork's workspace OR the parent repo. We turn hooks off and blank every config knob that
// shells out. Verified host-exec vectors: .git/hooks/* (and core.hooksPath), core.fsmonitor,
// core.pager, diff.external, and a forced commit.gpgsign with a planted gpg.program; the rest are
// defense in depth. Signing on land is re-enabled with trusted values appended after these (git's
// last -c for a key wins; see trustedSignArgs).
//
// A value coop reads then EXECUTES (or reads a host file from) — your editor, signing program,
// global excludesfile — must not come from the agent-writable repo at all: those use gitGlobalOut
// to read your trusted global config, never these helpers.
//
// The one residual gitHardening alone can't blank (the driver names are arbitrary) — an in-tree
// .gitattributes plus a fork-local filter/merge/diff driver that runs on the land rebase's
// checkout — is closed by forkDriverNeutralizer, which enumerates the fork's driver names and
// blanks each before that rebase. policyScan stays the human-facing backstop for the .gitattributes.
var gitHardening = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=",
	"-c", "core.sshCommand=",
	"-c", "core.pager=cat",
	"-c", "core.editor=true",
	"-c", "sequence.editor=true",
	"-c", "diff.external=",
	"-c", "uploadpack.packObjectsHook=",
	"-c", "protocol.ext.allow=never",
	"-c", "commit.gpgsign=false",
	"-c", "gpg.program=false",
	"-c", "gpg.ssh.program=false",
	"-c", "gpg.x509.program=false",
}

func gitBranch(dir string) string { return gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD") }

func gitDirty(dir string) bool { return gitOut(dir, "status", "--porcelain") != "" }

// forkDriverNeutralizer returns -c flags that blank every filter/merge/diff driver defined in the
// fork's OWN (local) git config, by name. gitHardening can't cover these — the driver names are
// arbitrary — but they're enumerable: an in-tree .gitattributes assigning `filter=x` (or merge/diff)
// to a path plus a fork-local filter.x.smudge / merge.x.driver / diff.x.command runs host code on the
// checkout/merge/diff of the land rebase. We read the fork's local driver names and blank each
// (filter.required=false so a blanked smudge doesn't hard-fail the checkout). A legit clone has no
// local filter/merge/diff config — those live in your global — so this blanks only what the agent
// planted; policyScan stays the human-facing backstop for the committed .gitattributes itself.
func forkDriverNeutralizer(ws string) []string {
	keys := gitOut(ws, "config", "--local", "--name-only", "--get-regexp", `^(filter|merge|diff)\.`)
	if keys == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, key := range strings.Split(keys, "\n") {
		var typ string
		for _, t := range []string{"filter", "merge", "diff"} {
			if strings.HasPrefix(key, t+".") {
				typ = t
				break
			}
		}
		if typ == "" {
			continue
		}
		rest := key[len(typ)+1:] // "<name>.<leaf>"
		dot := strings.LastIndex(rest, ".")
		if dot <= 0 {
			continue // a 2-part key (e.g. diff.external) has no <name> driver to neutralize
		}
		name := rest[:dot]
		if id := typ + "\x00" + name; !seen[id] {
			seen[id] = true
			switch typ {
			case "filter":
				out = append(out, "-c", "filter."+name+".smudge=", "-c", "filter."+name+".clean=",
					"-c", "filter."+name+".process=", "-c", "filter."+name+".required=false")
			case "merge":
				out = append(out, "-c", "merge."+name+".driver=")
			case "diff":
				out = append(out, "-c", "diff."+name+".command=", "-c", "diff."+name+".textconv=")
			}
		}
	}
	return out
}

// parseShortstat pulls insertion/deletion counts out of a `git diff --shortstat`
// line ("N files changed, I insertions(+), D deletions(-)").
func parseShortstat(s string) (ins, del int) {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		n := 0
		fmt.Sscanf(part, "%d", &n)
		switch {
		case strings.Contains(part, "insertion"):
			ins = n
		case strings.Contains(part, "deletion"):
			del = n
		}
	}
	return ins, del
}

// indent prefixes every line of s with two spaces.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// lastLines returns the last n lines of s (trailing blank lines trimmed first).
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// approve reports whether a destructive step is approved. --yes approves without
// asking; otherwise it prompts interactively. In a non-interactive run (no TTY)
// without --yes it refuses rather than silently taking the default — so a pipe or CI
// job can't land work and delete a fork on its own. Callers gate the whole command on
// this up front (with a clear "pass --yes" error); this is also the safe fallback.
func approve(prompt string, yes bool) bool {
	if yes {
		return true
	}
	if !ui.IsTerminal(os.Stdin) {
		return false
	}
	return confirm(prompt, true)
}

// hasYes reports whether args carry the -y/--yes confirmation-skip flag that destructive commands
// accept to run unattended (distinct from --force, which overrides a safety guard, not the prompt).
func hasYes(args []string) bool {
	for _, a := range args {
		if a == "-y" || a == "--yes" {
			return true
		}
	}
	return false
}

// destroyGate guards an UNRECOVERABLE deletion, returning nil only when it may proceed. With yes (the
// caller saw -y/--yes) it proceeds silently. Otherwise, piped (no TTY) it REFUSES — there's nothing
// to confirm against, so a script must opt in with --yes; at a TTY it asks "<what>? …" defaulting to
// No, so a stray Enter cancels. `what` names the blast radius, e.g. "delete task X (todo)". One gate
// for every rm (tasks, profiles, forks) so they can't drift. See rule destructive-confirm-gate.
func destroyGate(what string, yes bool) error {
	if yes {
		return nil
	}
	if !ui.IsTerminal(os.Stdin) {
		return fmt.Errorf("refusing to %s without confirmation — re-run with --yes (no terminal to prompt)", what)
	}
	if !confirm(what+"? this can't be undone", false) {
		return errors.New("cancelled")
	}
	return nil
}

// confirm asks a yes/no question, returning def with no tty (batch runs) or on a
// bare Enter.
func confirm(prompt string, def bool) bool {
	if !ui.IsTerminal(os.Stdin) {
		return def
	}
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	fmt.Fprintf(os.Stderr, "%s [%s] ", prompt, hint)
	var resp string
	fmt.Scanln(&resp)
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "":
		return def
	case "y", "yes":
		return true
	default:
		return false
	}
}
