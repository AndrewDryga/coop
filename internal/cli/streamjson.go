package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

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
	out  io.Writer         // rendered activity lines → terminal (+ fork log)
	tail io.Writer         // human text → the rate-limit detector's tail
	buf  []byte            // partial trailing line carried between Writes
	tool map[string]string // tool_use id → label, to name a failed tool_result
}

func newStreamDecoder(out, tail io.Writer) *streamDecoder {
	return &streamDecoder{out: out, tail: tail, tool: map[string]string{}}
}

func (d *streamDecoder) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	for {
		i := bytes.IndexByte(d.buf, '\n')
		if i < 0 {
			break
		}
		d.event(d.buf[:i])
		d.buf = d.buf[i+1:]
	}
	return len(p), nil
}

// flush renders any trailing line left without a newline. A well-formed NDJSON stream ends
// with one, so this is belt-and-suspenders for a truncated final event.
func (d *streamDecoder) flush() {
	if len(bytes.TrimSpace(d.buf)) > 0 {
		d.event(d.buf)
	}
	d.buf = nil
}

func (d *streamDecoder) emit(s string) { fmt.Fprintln(d.out, s) }

func (d *streamDecoder) toTail(s string) {
	if d.tail != nil && s != "" {
		fmt.Fprintln(d.tail, s)
	}
}

// event parses and renders one NDJSON line.
func (d *streamDecoder) event(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	var ev streamEvent
	if json.Unmarshal(raw, &ev) != nil {
		// Not an event (a stray diagnostic line) — show it as-is and let the tail see it,
		// rather than dropping output or crashing the run on an unexpected schema.
		d.emit(string(raw))
		d.toTail(string(raw))
		return
	}
	switch ev.Type {
	case "assistant":
		d.assistant(ev.Message)
	case "user":
		d.toolResult(ev.Message)
	case "rate_limit_event":
		d.rateLimit(ev.RateLimit)
	case "result":
		d.result(&ev)
	default:
		// system/init, session_state_changed, and any future type: nothing to show.
	}
}

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
				d.emit(t)
				d.toTail(t)
			}
		case "tool_use":
			glyph, label := toolDisplay(b.Name, b.Input)
			line := glyph + " " + b.Name
			if label != "" {
				line += " " + ui.Dim(truncate(label, 60))
			}
			d.emit(line)
			d.tool[b.ID] = strings.TrimSpace(b.Name + " " + label)
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
	d.toTail(fmt.Sprintf("Claude AI usage limit reached|%d", rl.ResetsAt))
}

// result renders the iteration's closing summary, or its error.
func (d *streamDecoder) result(ev *streamEvent) {
	if ev.IsError {
		msg := strings.TrimSpace(ev.Result)
		if msg == "" {
			msg = "error"
		}
		d.emit(ui.Red("✗ " + truncate(firstLine(msg), 80)))
		d.toTail(msg)
		return
	}
	dur := (time.Duration(ev.DurationMS) * time.Millisecond).Round(time.Second)
	d.emit(ui.Dim(fmt.Sprintf("· %d turns · %s · $%.2f", ev.NumTurns, dur, ev.TotalCostUSD)))
	if t := strings.TrimSpace(ev.Result); t != "" {
		d.toTail(t) // the final message, in case it carries a limit notice
	}
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

// toolDisplay picks a glyph and a one-line summary for a tool call from its input.
func toolDisplay(name string, input json.RawMessage) (glyph, label string) {
	var in toolInput
	_ = json.Unmarshal(input, &in)
	switch name {
	case "Bash":
		return "⚙", firstLine(in.Command)
	case "Edit", "Write", "NotebookEdit":
		return "✎", in.FilePath
	case "Read":
		return "▸", in.FilePath
	case "Grep", "Glob":
		return "⌕", in.Pattern
	default:
		return "·", in.Description
	}
}

// firstLine is s trimmed to its first non-empty line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
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
	Message      json.RawMessage `json:"message"`
	RateLimit    *rateLimitInfo  `json:"rate_limit_info"`
	IsError      bool            `json:"is_error"`
	Result       string          `json:"result"`
	NumTurns     int             `json:"num_turns"`
	DurationMS   int             `json:"duration_ms"`
	TotalCostUSD float64         `json:"total_cost_usd"`
}

type rateLimitInfo struct {
	Status        string `json:"status"`
	ResetsAt      int64  `json:"resetsAt"`
	RateLimitType string `json:"rateLimitType"`
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
	Command     string `json:"command"`
	FilePath    string `json:"file_path"`
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
}
