package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

// llmIcon marks a line of the agent's own narration (vs a tool call or coop's own output).
const llmIcon = "✦"

// streamDecoder renders Claude Code's `--output-format stream-json` NDJSON into compact,
// human-readable activity lines as the agent works, so a loop iteration shows what it is
// doing instead of going silent until the final message. It implements io.Writer to sit
// where the box pipes the agent's stdout: each newline-terminated chunk is one JSON event.
//
// Human text (assistant messages, the final result, and a limit notice translated from the
// structured rate_limit_event) is also copied to tail, so the loop's rate-limit detector —
// which greps the agent's text — keeps working under stream-json. Tool inputs and outputs
// are deliberately NOT sent to tail: they're the agent's own work and can contain strings
// like "429" that would false-match the limit markers.
type streamDecoder struct {
	*ndjsonDecoder
	agent      string            // the agent whose stream this is (e.g. claude), for the model line
	profile    string            // the credential profile in play, for the model line
	root       string            // the repo's in-box mount; tool paths show relative to it (empty = off)
	tool       map[string]string // tool_use id → label, to name a failed tool_result
	last       *iterResult       // the last result event's cost/turns/tokens, for the loop's telemetry
	limitShown bool              // a blocking structured limit already owns the visible notice
}

func newStreamDecoder(out, tail io.Writer, agent, profile, root string) *streamDecoder {
	d := &streamDecoder{agent: agent, profile: profile, root: root, tool: map[string]string{}}
	d.ndjsonDecoder = newNDJSONDecoder(out, tail, d.event)
	return d
}

// iterationStreamDecoder is the stdout seam runIteration needs from every provider decoder.
// The concrete type remains provider-specific so each stream schema stays small and explicit.
type iterationStreamDecoder interface {
	io.Writer
	flush()
	lastIterResult() *iterResult
}

// newIterationStreamDecoder dispatches on the schema declared by the provider adapter. Several
// CLIs use overlapping flag names, so the flags themselves cannot identify the event schema.
func newIterationStreamDecoder(agent string, out, tail io.Writer, profile, root, model string) iterationStreamDecoder {
	adapter, ok := agents.Get(agent)
	if !ok {
		return nil
	}
	switch adapter.Stream().Format {
	case agents.StreamClaudeJSON:
		return newStreamDecoder(out, tail, agent, profile, root)
	case agents.StreamCodexJSON:
		return newCodexStreamDecoder(out, tail, agent, profile, root, model)
	case agents.StreamGeminiJSON:
		return newGeminiStreamDecoder(out, tail, agent, profile, root, model)
	case agents.StreamGrokJSON:
		return newGrokStreamDecoder(out, tail, agent, profile, root, model)
	default:
		return nil
	}
}

// ndjsonDecoder owns the byte-stream mechanics shared by every provider: partial lines across
// Writes, final-line flushing, and raw passthrough for diagnostics that are not JSON events.
// Provider decoders only handle complete, valid JSON values.
type ndjsonDecoder struct {
	out       io.Writer
	tail      io.Writer
	buf       []byte
	event     func(json.RawMessage)
	beforeRaw func()
}

func newNDJSONDecoder(out, tail io.Writer, event func(json.RawMessage)) *ndjsonDecoder {
	return &ndjsonDecoder{out: out, tail: tail, event: event}
}

func (d *ndjsonDecoder) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	for {
		i := bytes.IndexByte(d.buf, '\n')
		if i < 0 {
			break
		}
		d.line(d.buf[:i])
		d.buf = d.buf[i+1:]
	}
	return len(p), nil
}

// flush renders any trailing line left without a newline. A well-formed NDJSON stream ends
// with one, so this is belt-and-suspenders for a truncated final event.
func (d *ndjsonDecoder) flush() {
	if len(bytes.TrimSpace(d.buf)) > 0 {
		d.line(d.buf)
	}
	d.buf = nil
}

func (d *ndjsonDecoder) emit(s string) { fmt.Fprintln(d.out, s) }

func (d *ndjsonDecoder) toTail(s string) {
	if d.tail != nil && s != "" {
		fmt.Fprintln(d.tail, s)
	}
}

func (d *ndjsonDecoder) line(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	if !json.Valid(raw) {
		d.passthrough(raw)
		return
	}
	d.event(json.RawMessage(raw))
}

func (d *ndjsonDecoder) rawLine(raw []byte) {
	s := string(bytes.TrimSpace(raw))
	d.emit(s)
	d.toTail(s)
}

func (d *ndjsonDecoder) passthrough(raw []byte) {
	if d.beforeRaw != nil {
		d.beforeRaw()
	}
	d.rawLine(raw)
}

// event parses and renders one NDJSON line.
func (d *streamDecoder) event(raw json.RawMessage) {
	var ev streamEvent
	if json.Unmarshal(raw, &ev) != nil {
		// Not an event (a stray diagnostic line) — show it as-is and let the tail see it,
		// rather than dropping output or crashing the run on an unexpected schema.
		d.passthrough(raw)
		return
	}
	switch ev.Type {
	case "assistant":
		d.assistant(ev.Message)
	case "user":
		d.toolResult(ev.Message)
	case "system":
		d.system(&ev)
	case "rate_limit_event":
		d.rateLimit(ev.RateLimit)
	case "result":
		d.result(&ev)
	default:
		// session_state_changed and any future type: nothing to show.
	}
}

func (d *streamDecoder) lastIterResult() *iterResult { return d.last }

// assistant renders an assistant turn's content: visible text (the agent talking) and tool
// calls. Thinking blocks are skipped to keep the view about what's being done.
func (d *streamDecoder) assistant(msg json.RawMessage) {
	var m streamMessage
	if json.Unmarshal(msg, &m) != nil {
		return
	}
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				if !d.limitShown || !streamLimitNotice(t) {
					d.emit(ui.Magenta(llmIcon) + " " + t) // mark the agent's own voice
				}
				d.toTail(t) // the tail (limit detection) always gets the plain text
			}
		case "tool_use":
			glyph, displayName, label, outside := toolDisplay(d.root, b.Name, b.Input)
			line := glyph + " " + displayName
			if label != "" {
				shown := truncate(label, 60)
				if outside {
					// The agent reached outside the repo tree — flag it (⚠) and highlight the
					// path yellow, vs the dim repo-relative path an in-tree call shows.
					line += " " + ui.Yellow("⚠ "+shown)
				} else {
					line += " " + ui.Dim(shown)
				}
			}
			d.emit(line)
			d.tool[b.ID] = strings.TrimSpace(displayName + " " + label)
		}
	}
}

// toolResult flags a failed tool call; a success is left implied by the next action, to keep
// the stream from doubling every step with a checkmark line.
func (d *streamDecoder) toolResult(msg json.RawMessage) {
	var m streamMessage
	if json.Unmarshal(msg, &m) != nil {
		return
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" || !b.IsError {
			continue
		}
		line := "  " + ui.Red("✗")
		if label := d.tool[b.ToolUseID]; label != "" {
			line += " " + label
		}
		if first := firstLine(rawText(b.Content)); first != "" {
			line += ": " + truncate(first, 60)
		}
		d.emit(line)
	}
}

// system renders the init event's model line, so each loop iteration shows which model is
// actually working — the agent's default, a --model override, or whatever the account tier
// resolves to. coop doesn't pick the model, so the agent's own init report is the one
// reliable source; it lands right after the iteration banner, before the agent's first move.
// The id is shown verbatim, suffix and all — e.g. the `[1m]` 1M-context tier reads like an
// ANSI bold code (ESC[1m) but is literal text; normalizing it would risk misrepresenting the
// model, so we deliberately don't.
func (d *streamDecoder) system(ev *streamEvent) {
	if ev.Subtype == "init" && ev.Model != "" {
		d.emit(streamModelLine(d.agent, ev.Model, d.profile))
	}
}

func streamModelLine(agent, model, profile string) string {
	if agent == "" {
		return ui.Dim("· model " + model)
	}
	// Dim the labels (· using / model / credential) but leave the values — agent, model, credential —
	// at normal brightness, so they stand out a touch against the otherwise-faint line.
	line := ui.Dim("· using ") + agent + ui.Dim(" model ") + model
	if profile != "" {
		line += ui.Dim(" credential ") + profile
	}
	return line
}

// rateLimit shows a real limit and translates it into the text the loop's detector already
// understands, carrying the reset epoch so the loop sleeps until then rather than backing off
// blindly. Informational "allowed"/"warning" events (emitted on normal runs) are ignored.
func (d *streamDecoder) rateLimit(rl *rateLimitInfo) {
	if rl == nil || !blockingLimitStatus(rl.Status) {
		return
	}
	when := ""
	if rl.ResetsAt > 0 {
		when = " — resets " + time.Unix(rl.ResetsAt, 0).Format("Jan 2, 3:04pm")
	}
	d.emit(ui.Yellow("⚠ rate limited") + " (" + rl.RateLimitType + ")" + when)
	d.limitShown = true
	d.toTail(fmt.Sprintf("Claude AI usage limit reached|%d", rl.ResetsAt))
}

func streamLimitNotice(s string) bool {
	return hitLimitRe.MatchString(s) || strings.Contains(strings.ToLower(s), "usage limit reached")
}

// result renders the iteration's closing summary, or its error.
func (d *streamDecoder) result(ev *streamEvent) {
	if ev.IsError {
		msg := strings.TrimSpace(ev.Result)
		if msg == "" {
			msg = "error"
		}
		if !d.limitShown || !streamLimitNotice(msg) {
			d.emit(ui.Red("✗ " + truncate(firstLine(msg), 80)))
		}
		d.toTail(msg)
		return
	}
	dur := (time.Duration(ev.DurationMS) * time.Millisecond).Round(time.Second)
	res := &iterResult{CostUSD: ev.TotalCostUSD, Turns: ev.NumTurns, DurationMS: ev.DurationMS}
	line := fmt.Sprintf("· %d turns · %s · $%.2f", ev.NumTurns, dur, ev.TotalCostUSD)
	if ev.Usage != nil {
		res.InTok, res.OutTok = ev.Usage.inputTotal(), ev.Usage.OutputTokens
		line += " · " + tokenUsage(res.InTok, res.OutTok)
	}
	d.last = res
	d.emit(ui.Dim(line))
	if t := strings.TrimSpace(ev.Result); t != "" {
		d.toTail(t) // the final message, in case it carries a limit notice
	}
}

// humanTokens renders a token count compactly: 4243→"4.2k", 1_234_567→"1.2M", <1000 verbatim.
func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func tokenUsage(input, output int) string {
	return fmt.Sprintf("%s input / %s output", humanTokens(input), humanTokens(output))
}

// blockingLimitStatus reports whether a rate_limit_event status means the agent is actually
// blocked, vs the informational "allowed"/"warning" events every run emits. The blocking
// values are the rate-limit states in the Claude CLI bundle; an unknown status is treated as
// non-blocking and left to the textual detector as a backstop.
func blockingLimitStatus(s string) bool {
	switch s {
	case "blocked", "rejected", "exhausted", "throttled":
		return true
	}
	return false
}

// toolDisplay picks a glyph, display verb, and one-line summary for a tool call from its input. For
// file tools it shows the path repo-relative (against root) and reports outside=true when the path
// escapes the repo tree, so the caller can flag it. Non-path tools are never "outside".
func toolDisplay(root, name string, input json.RawMessage) (glyph, displayName, label string, outside bool) {
	var in toolInput
	_ = json.Unmarshal(input, &in)
	switch name {
	case "Bash":
		command := stripLeadingCD(in.Command)
		if glyph, displayName, label, ok := consultDelegateDisplay(command); ok {
			return glyph, displayName, label, false
		}
		if glyph, displayName, label, ok := taskTransitionDisplay(command); ok {
			return glyph, displayName, label, false
		}
		if glyph, displayName, label, ok := taskMkdirDisplay(command); ok {
			return glyph, displayName, label, false
		}
		return "⚙", name, firstLine(relativizeRoot(root, command)), false
	case "Task":
		detail := strings.TrimSpace(in.SubagentType)
		if description := strings.TrimSpace(in.Description); description != "" {
			if detail != "" {
				detail += ": "
			}
			detail += description
		}
		if detail != "" {
			detail = "→ " + detail
		}
		return "⌥", "subagent", detail, false
	case "Edit", "Write":
		if glyph, displayName, label, ok := taskFileDisplay(name, in.FilePath); ok {
			return glyph, displayName, label, false
		}
		rel, inside := repoRel(root, in.FilePath)
		return "✎", name, rel, !inside
	case "NotebookEdit":
		rel, inside := repoRel(root, in.FilePath)
		return "✎", name, rel, !inside
	case "Read":
		rel, inside := repoRel(root, in.FilePath)
		return "▸", name, rel, !inside
	case "Grep", "Glob":
		return "⌕", name, in.Pattern, false
	default:
		return "·", name, in.Description, false
	}
}

// consultDelegateDisplay recognizes the role-addressed wrappers the lead invokes through Bash.
// Their grammar is intentionally simple here: command name, optional flag tokens, then role. This
// is display-only classification, not shell parsing; malformed or unrelated commands fall through.
func consultDelegateDisplay(command string) (glyph, displayName, label string, ok bool) {
	fields := strings.Fields(command)
	if len(fields) < 2 {
		return "", "", "", false
	}
	switch fields[0] {
	case "coop-consult":
		glyph, displayName = "☎", "consult"
	case "coop-delegate":
		glyph, displayName = "⇢", "delegate"
	default:
		return "", "", "", false
	}
	for _, field := range fields[1:] {
		if strings.HasPrefix(field, "-") {
			continue
		}
		return glyph, displayName, "→ " + field, true
	}
	return "", "", "", false
}

type streamedTaskPath struct {
	state  string
	id     string
	file   string
	nested bool
}

// streamedTaskPaths extracts only the queue path shape the in-box contract tells agents to use.
// It scans command text rather than parsing shell syntax, so quoted paths and absolute mount
// prefixes work while unrelated mv/mkdir commands remain ordinary Bash activity.
func streamedTaskPaths(text string) []streamedTaskPath {
	const marker = ".agent/tasks/"
	var refs []streamedTaskPath
	for from := 0; from < len(text); {
		i := strings.Index(text[from:], marker)
		if i < 0 {
			break
		}
		from += i + len(marker)
		tail := text[from:]
		slash := strings.IndexByte(tail, '/')
		if slash < 0 {
			continue
		}
		state := tail[:slash]
		if !streamedTaskState(state) {
			continue
		}
		rest := tail[slash+1:]
		end := streamedTaskTokenEnd(rest)
		if end == 0 {
			continue
		}
		ref := streamedTaskPath{state: state, id: rest[:end]}
		if end < len(rest) && rest[end] == '/' {
			ref.nested = true
			fileRest := rest[end+1:]
			fileEnd := streamedTaskTokenEnd(fileRest)
			if fileEnd > 0 && (fileEnd == len(fileRest) || fileRest[fileEnd] != '/') {
				switch fileRest[:fileEnd] {
				case "task.md", "log.md", "state.md", "decision.md":
					ref.file = strings.TrimSuffix(fileRest[:fileEnd], ".md")
				}
			}
		}
		refs = append(refs, ref)
	}
	return refs
}

func streamedTaskState(state string) bool {
	for _, known := range taskStates {
		if state == known {
			return true
		}
	}
	return false
}

func streamedTaskTokenEnd(s string) int {
	if i := strings.IndexAny(s, "/ \t\r\n\"'`;|&<>()"); i >= 0 {
		return i
	}
	return len(s)
}

func taskTransitionDisplay(command string) (glyph, displayName, label string, ok bool) {
	line := strings.TrimSpace(firstLine(command))
	if line != "mv" && !strings.HasPrefix(line, "mv ") {
		return "", "", "", false
	}
	refs := streamedTaskPaths(line)
	if len(refs) < 2 || refs[0].nested || refs[1].nested || refs[0].id != refs[1].id || refs[0].state == refs[1].state {
		return "", "", "", false
	}
	src, dst := refs[0], refs[1]
	if src.state == stateBlocked && (dst.state == stateTodo || dst.state == stateInProgress) {
		return "↺", "unblock", readableTaskID(dst.id), true
	}
	switch dst.state {
	case stateInProgress:
		return "⇢", "claim", readableTaskID(dst.id), true
	case stateDone:
		return "✓", "done", readableTaskID(dst.id), true
	case stateBlocked:
		return "⏸", "block", readableTaskID(dst.id), true
	case stateTodo:
		return "＋", "queue", readableTaskID(dst.id), true
	}
	return "", "", "", false
}

func taskMkdirDisplay(command string) (glyph, displayName, label string, ok bool) {
	line := strings.TrimSpace(firstLine(command))
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "mkdir" {
		return "", "", "", false
	}
	hasParents := false
	for _, field := range fields[1:] {
		if field == "--parents" || (strings.HasPrefix(field, "-") && !strings.HasPrefix(field, "--") && strings.Contains(field[1:], "p")) {
			hasParents = true
			break
		}
	}
	refs := streamedTaskPaths(line)
	if !hasParents || len(refs) == 0 || refs[0].nested {
		return "", "", "", false
	}
	return "·", "prepare", readableTaskID(refs[0].id), true
}

func taskFileDisplay(toolName, path string) (glyph, displayName, label string, ok bool) {
	refs := streamedTaskPaths(filepath.ToSlash(path))
	if len(refs) != 1 || refs[0].file == "" {
		return "", "", "", false
	}
	ref := refs[0]
	if toolName == "Write" && ref.state == stateTodo && ref.file == "task" {
		return "＋", "queue", readableTaskID(ref.id), true
	}
	return "✎", ref.file, readableTaskID(ref.id), true
}

func readableTaskID(id string) string {
	if len(id) > len("2006-01-02-") && id[4] == '-' && id[7] == '-' && id[10] == '-' {
		for _, i := range []int{0, 1, 2, 3, 5, 6, 8, 9} {
			if id[i] < '0' || id[i] > '9' {
				return id
			}
		}
		return id[len("2006-01-02-"):]
	}
	return id
}

// repoRel renders an absolute in-box file path relative to the repo root when it falls inside
// it, so a tool line reads `internal/cli/streamjson.go` instead of the whole mount path. A path
// OUTSIDE the repo is returned unchanged with inside=false (the caller flags it); a non-absolute
// path — already short and relative to the agent's cwd, the repo — is left as-is. root is the
// repo's in-box mount (box.Workdir); an empty root disables both relativizing and flagging.
func repoRel(root, p string) (rel string, inside bool) {
	if root == "" || p == "" || !filepath.IsAbs(p) {
		return p, true
	}
	r, err := filepath.Rel(root, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return p, false // escapes the repo tree
	}
	return r, true
}

// relativizeRoot removes the repeated in-box repo mount prefix from command text. Bash calls can
// carry several absolute repo paths, so this deliberately stays a plain all-occurrences replace;
// root plus the path separator keeps sibling paths such as <root>-other intact.
func relativizeRoot(root, command string) string {
	if root == "" {
		return command
	}
	return strings.ReplaceAll(command, root+string(os.PathSeparator), "")
}

// firstLine is s trimmed to its first non-empty line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// diagnosticTailMax bounds how far back into aggregated command output we scan for a
// compact failure line. Failures usually land near the end; the full log still has everything.
const diagnosticTailMax = 24

// commandFailureDiagnostic picks one meaningful line from a failed command's aggregated
// output for the compact live view. It strips empty/ANSI-only lines, scans a bounded tail,
// prefers failure-shaped lines over generic summaries, and falls back to the last informative
// line. Empty input yields "". Callers truncate for display width.
func commandFailureDiagnostic(output string) string {
	lines := diagnosticLines(output)
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > diagnosticTailMax {
		lines = lines[len(lines)-diagnosticTailMax:]
	}
	// Prefer a failure-shaped line that is not just a weak summary (bare FAIL, make Error N).
	for i := len(lines) - 1; i >= 0; i-- {
		if looksLikeFailure(lines[i]) && !isWeakDiagnostic(lines[i]) {
			return lines[i]
		}
	}
	// Any failure-shaped line, including weak ones.
	for i := len(lines) - 1; i >= 0; i-- {
		if looksLikeFailure(lines[i]) {
			return lines[i]
		}
	}
	return lines[len(lines)-1]
}

// diagnosticLines splits output into cleaned, non-empty lines (ANSI stripped, controls dropped).
func diagnosticLines(output string) []string {
	if output == "" {
		return nil
	}
	raw := strings.Split(output, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		line = cleanDiagnosticLine(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// cleanDiagnosticLine strips ANSI CSI sequences and other C0 controls, normalizes tabs, and trims.
func cleanDiagnosticLine(s string) string {
	s = stripANSISequences(s)
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}

// stripANSISequences removes CSI / other ESC sequences and drops remaining C0 controls (except
// tab, handled by the caller). Keeps the visible text of colored compiler/test output.
func stripANSISequences(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
					j++
				}
				if j < len(s) {
					j++ // final byte (e.g. 'm')
				}
				i = j
				continue
			}
			if j < len(s) {
				i = j + 1
				continue
			}
			break
		}
		c := s[i]
		// Drop C0 controls except tab (callers normalize tabs to spaces).
		if (c < 0x20 && c != '\t') || c == 0x7f {
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// looksLikeFailure reports whether a cleaned line carries failure-shaped content.
func looksLikeFailure(line string) bool {
	if strings.HasPrefix(line, "--- FAIL") || strings.HasPrefix(line, "FAIL") {
		return true
	}
	// Compiler/test locations: path:line:… or path:line:col:…
	if diagnosticLocation(line) {
		return true
	}
	lower := strings.ToLower(line)
	for _, nonFailure := range []string{
		"0 errors", "no errors", "errors: 0", "0 failed", "no failures", "failed: 0",
	} {
		if strings.Contains(lower, nonFailure) {
			return false
		}
	}
	for _, m := range []string{
		"error", "failed", "failure", "panic:", "fatal", "undefined",
		"cannot ", "can't ", "denied", "refused", "not found", "no such",
		"exit status", "exit code", "err:", "fail:",
	} {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// diagnosticLocation is true for lines that look like file:line: messages (go test, compilers).
func diagnosticLocation(line string) bool {
	// Require something:digits: so "ok: done" and "FAIL pkg 0.1s" stay out.
	i := strings.IndexByte(line, ':')
	if i <= 0 || i+1 >= len(line) {
		return false
	}
	// digit after first colon (line number)
	if line[i+1] < '0' || line[i+1] > '9' {
		return false
	}
	// path-ish before colon: has / or .go / .rs / .ts etc, or starts with ./
	head := line[:i]
	if strings.ContainsAny(head, "/\\") || strings.Contains(head, ".go") ||
		strings.HasPrefix(head, "./") || strings.HasPrefix(head, "../") {
		return true
	}
	// "foo_test.go:12: …" — bare file name with extension
	if j := strings.LastIndexByte(head, '.'); j > 0 && j < len(head)-1 {
		return true
	}
	return false
}

// isWeakDiagnostic marks generic exit/summary lines that are less useful than a nearby detail.
func isWeakDiagnostic(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	switch lower {
	case "fail", "failed", "failure", "error", "errors", "fatal", "panic",
		"command failed", "tests failed", "build failed", "error: command failed",
		"error: tests failed", "error: build failed":
		return true
	}
	if strings.Contains(lower, "process completed with exit code") ||
		strings.Contains(lower, "command failed with exit code") {
		return true
	}
	// go test package rollup: "FAIL pkg 0.1s" (no assertion detail)
	if strings.HasPrefix(line, "FAIL ") || strings.HasPrefix(line, "FAIL\t") {
		if !strings.Contains(line, ":") {
			return true
		}
	}
	// make: *** [target] Error N
	if strings.HasPrefix(lower, "make:") && strings.Contains(lower, "error") {
		return true
	}
	// *** … exit summary
	if strings.HasPrefix(line, "***") {
		return true
	}
	// bare "exit status N" / "exit code N"
	if strings.HasPrefix(lower, "exit status ") || strings.HasPrefix(lower, "exit code ") {
		return true
	}
	return false
}

// stripLeadingCD removes leading `cd <dir> &&` clauses (or a first line that's only `cd <dir>`) so a
// streamed Bash line shows the command that did the work, not the chdir an agent prefixes to reach a
// monorepo subdir — otherwise a whole run reads as identical `cd …/portal` lines. A bare `cd <dir>`
// with nothing after it is left as-is (that IS the command).
func stripLeadingCD(cmd string) string {
	for {
		s := strings.TrimLeft(cmd, " \t")
		if !strings.HasPrefix(s, "cd ") {
			return cmd
		}
		cut := -1
		if i := strings.Index(s, " && "); i >= 0 { // one-liner: `cd X && rest`
			cut = i + len(" && ")
		}
		if i := strings.IndexByte(s, '\n'); i >= 0 && (cut < 0 || i < cut) { // multi-line: `cd X` then rest
			cut = i + 1
		}
		if cut < 0 {
			return cmd
		}
		rest := strings.TrimSpace(s[cut:])
		if rest == "" {
			return cmd
		}
		cmd = rest // loop to peel another leading cd (e.g. `cd a && cd b && cmd`)
	}
}

// rawText renders a tool_result's content — a JSON string, or an array of {type,text} blocks
// — as plain text, best-effort.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			b.WriteString(x.Text)
		}
		return b.String()
	}
	return ""
}

type streamEvent struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	Model        string          `json:"model"`
	Message      json.RawMessage `json:"message"`
	RateLimit    *rateLimitInfo  `json:"rate_limit_info"`
	IsError      bool            `json:"is_error"`
	Result       string          `json:"result"`
	NumTurns     int             `json:"num_turns"`
	DurationMS   int             `json:"duration_ms"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	Usage        *usageInfo      `json:"usage"`
}

type rateLimitInfo struct {
	Status        string `json:"status"`
	ResetsAt      int64  `json:"resetsAt"`
	RateLimitType string `json:"rateLimitType"`
}

// usageInfo is the token accounting on Claude Code's result event. input_tokens is fresh
// (uncached) input; the two cache fields are input written to / read from the prompt cache; a
// token-use view sums all three as "input". server_tool_use (web search/fetch counts) is ignored.
type usageInfo struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// inputTotal sums every input-side token (fresh + cache write + cache read) — the "in" half of the
// in/out the iteration line and the cost table show.
func (u usageInfo) inputTotal() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// iterResult is the closing tally the loop reads off the decoder after a box run — the result
// event's cost, turns, and token totals — so it can attribute cost to the task in telemetry. nil
// until a (non-error) result event lands; an interrupted run leaves it nil.
type iterResult struct {
	CostUSD    float64
	Turns      int
	DurationMS int
	InTok      int
	OutTok     int
}

type streamMessage struct {
	Content []streamBlock `json:"content"`
}

type streamBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

type toolInput struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	Pattern      string `json:"pattern"`
	Description  string `json:"description"`
	SubagentType string `json:"subagent_type"`
}
