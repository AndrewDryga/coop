package tasks

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/ui"
)

// newSupervisorID mints a fresh random id to embed in a completion window's lock name, so nothing
// else can collide with it. internal/cli/acp_cmd.go keeps its own copy (its ACP supervisor's box
// label needs the identical shape) rather than exporting one across the package boundary — the
// same "local-redeclare a trivial, stateless leaf" shape gitOut uses; see git.go.
func newSupervisorID() (string, error) {
	idbuf := make([]byte, 8)
	if _, err := rand.Read(idbuf); err != nil {
		return "", err
	}
	return hex.EncodeToString(idbuf), nil
}

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

// readFileString returns a file's contents, or "" if it can't be read.
func readFileString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
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

// paintCount renders a count, applying paint only when it's nonzero so a zero stays
// plain — a "0 blocked" shouldn't read as an alarm. Shared by the `coop tasks` summary
// and the loop banner (internal/cli/util.go keeps its own copy for the same reason gitOut
// does — see git.go).
func paintCount(v int, paint func(string) string) string {
	if v > 0 {
		return paint(strconv.Itoa(v))
	}
	return strconv.Itoa(v)
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
// for every rm (tasks, profiles, forks) so they can't drift — internal/cli/util.go keeps its own copy
// (the fork/profile verbs stay there) for the same reason gitOut does; see git.go.
//
// An interactive flow that already owns its input scanner may provide one ask callback. That keeps
// the destructive decision in this gate without making the flow compete with fmt.Scanln for stdin.
func destroyGate(what string, yes bool, asks ...func(string) bool) error {
	if yes {
		return nil
	}
	if len(asks) > 1 {
		return errors.New("destroy gate accepts at most one prompt callback")
	}
	if len(asks) == 1 {
		if !asks[0](what + "? this can't be undone") {
			return errors.New("cancelled")
		}
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
	return confirmationResponse(resp, def)
}

// confirmationResponse applies the shared y/N parsing after a caller has read a response.
func confirmationResponse(resp string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "":
		return def
	case "y", "yes":
		return true
	default:
		return false
	}
}

// copyFile copies src to dst, both regular files. Used to seed a fork worktree with a folder-mode
// task tree (copyTree) and to write per-fork task slices.
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

// lastLines returns the last n lines of s (trailing blank lines trimmed first).
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// truncate shortens s to n runes, marking elision with an ellipsis. internal/cli/util.go keeps its
// own copy (used well beyond the task views) for the same reason gitOut does; see git.go.
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

// padRight right-pads s to w columns counted in RUNES — unlike fmt's %-Ns, which counts bytes and
// so mis-pads a value carrying a multibyte glyph (e.g. a truncated name's "…"). internal/cli/util.go
// keeps its own copy (used well beyond the task views) for the same reason gitOut does; see git.go.
func padRight(s string, w int) string {
	if n := utf8.RuneCountInString(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
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

// nearestCommand suggests the candidate closest to a mistyped command — see internal/cli/util.go's
// copy (kept separately for the same reason gitOut is; see git.go) for the full distance-scaling
// rationale.
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

// unknownErr is the one shape for a rejected subcommand / value: `unknown <noun> "<token>" — use: a,
// b, c`, with a "did you mean X?" when the token is a near-miss. internal/cli/util.go keeps its own
// copy (shared by its own non-task sub-command groups) for the same reason gitOut does; see git.go.
func unknownErr(noun, token string, valid []string) error {
	if guess, ok := nearestCommand(token, valid); ok {
		return fmt.Errorf("unknown %s %q — use: %s (did you mean %q?)", noun, token, strings.Join(valid, ", "), guess)
	}
	return fmt.Errorf("unknown %s %q — use: %s", noun, token, strings.Join(valid, ", "))
}

// flagValue parses one `--flag value` or `--flag=value` occurrence at args[i]. ok reports whether
// args[i] names flag at all; ok can be true with a non-nil err (e.g. a bare trailing --flag with no
// value). consumed is how many args[i:] elements the flag+value occupy (1 for `--flag=value`, 2 for
// `--flag value`) — internal/cli/commands.go keeps its own copy (used across many non-task flags) for
// the same reason gitOut does; see git.go.
func flagValue(args []string, i int, flag string) (val string, consumed int, ok bool, err error) {
	switch a := args[i]; {
	case a == flag:
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
			return "", 0, true, fmt.Errorf("%s needs a value", flag)
		}
		return args[i+1], 2, true, nil
	case strings.HasPrefix(a, flag+"="):
		if v := strings.TrimPrefix(a, flag+"="); v != "" {
			return v, 1, true, nil
		}
		return "", 0, true, fmt.Errorf("%s needs a value", flag)
	}
	return "", 0, false, nil
}
