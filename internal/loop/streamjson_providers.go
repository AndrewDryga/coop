package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

const (
	geminiColorWarning = "Warning: 256-color support not detected. Using a terminal with at least 256-color support is recommended for a better visual experience."
	geminiYOLOWarning  = "YOLO mode is enabled. All tool calls will be automatically approved."
	codexRouterError   = "ERROR codex_core::tools::router:"
	codexStdinBanner   = "Reading additional input from stdin..."
	codexReviewFooter  = "tokens used"
	// This marker is returned only through the internal response tail. It keeps an adapter-owned
	// envelope that failed validation from accidentally reaching a later valid-looking receipt.
	codexMalformedReviewEnvelope = "\x00coop-codex-review-envelope-invalid\x00"
)

// normalizeCodexReviewOutput removes only the footer shape emitted by the Codex transport wrapper.
// The wrapper appends `tokens used` plus a formatted count and may repeat the final agent message
// or its exact terminal evidence/receipt block before or after that footer. Earlier agent messages
// are narration, not part of the echoed response. A different trailing block is not a second answer
// to merge: it is an invalid envelope and gets an impossible terminal marker so strict receipt
// parsing fails.
func normalizeCodexReviewOutput(output string) (string, bool) {
	output = normalizeReviewVerdictOutput(output)
	lines := strings.Split(output, "\n")
	end := len(lines) - 1
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if end < 0 {
		return output, true
	}

	footer := -1
	for i := 0; i < end; i++ {
		if lines[i] != codexReviewFooter {
			continue
		}
		if !codexTokenCount(lines[i+1]) {
			return rejectCodexReviewEnvelope(output)
		}
		if footer >= 0 {
			return rejectCodexReviewEnvelope(output)
		}
		footer = i
	}
	if end >= 0 && lines[end] == codexReviewFooter {
		return rejectCodexReviewEnvelope(output)
	}
	if footer < 0 {
		if strings.Contains(output, "REVIEW COMPLETE — ") {
			if _, ok := reviewReopenReceipt(output); !ok {
				return rejectCodexReviewEnvelope(output)
			}
			return output, true
		}
		return output, true
	}

	beforeFooter := lines[:footer]
	afterFooter := strings.Join(lines[footer+2:end+1], "\n")
	if afterFooter != "" {
		if validCodexReviewCandidate(afterFooter) && !hasCodexStructuredReviewLine(beforeFooter) {
			return afterFooter, true
		}
		if !validCodexReviewCandidate(afterFooter) ||
			codexReviewReceiptCount(beforeFooter)+codexReviewReceiptCount(strings.Split(afterFooter, "\n")) != 2 ||
			!hasCodexReviewSuffix(beforeFooter, afterFooter) {
			return rejectCodexReviewEnvelope(output)
		}
		return afterFooter, true
	}
	if canonical, ok := codexReviewCandidate(beforeFooter); ok {
		return canonical, true
	}
	if canonical, ok := duplicatedCodexReviewSuffix(beforeFooter); ok {
		return canonical, true
	}
	return rejectCodexReviewEnvelope(output)
}

func hasCodexStructuredReviewLine(lines []string) bool {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "AUDIT EVIDENCE — ") || strings.Contains(line, "REVIEW COMPLETE — ") {
			return true
		}
	}
	return false
}

func codexReviewCandidate(lines []string) (string, bool) {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || codexReviewReceiptCount(lines) != 1 {
		return "", false
	}
	candidate := strings.Join(lines, "\n")
	if _, valid := reviewReopenReceipt(candidate); !valid {
		return "", false
	}
	return candidate, true
}

func validCodexReviewCandidate(candidate string) bool {
	_, ok := codexReviewCandidate(strings.Split(candidate, "\n"))
	return ok
}

func codexReviewReceiptCount(lines []string) int {
	count := 0
	for _, line := range lines {
		if _, valid := parseReviewReceiptLine(line); valid {
			count++
		}
	}
	return count
}

func hasCodexReviewSuffix(lines []string, candidate string) bool {
	before := strings.Join(lines, "\n")
	return before == candidate || strings.HasSuffix(before, "\n"+candidate)
}

func duplicatedCodexReviewSuffix(lines []string) (string, bool) {
	if codexReviewReceiptCount(lines) != 2 {
		return "", false
	}
	for start := 1; start < len(lines); start++ {
		candidate, ok := codexReviewCandidate(lines[start:])
		if ok && hasCodexReviewSuffix(lines[:start], candidate) {
			return candidate, true
		}
	}
	return "", false
}

func rejectCodexReviewEnvelope(output string) (string, bool) {
	return strings.TrimRight(output, "\r\n") + "\n" + codexMalformedReviewEnvelope, false
}

func codexTokenCount(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	groups := strings.Split(value, ",")
	if len(groups) > 1 && (len(groups[0]) < 1 || len(groups[0]) > 3) {
		return false
	}
	for i, group := range groups {
		if group == "" || (i > 0 && len(group) != 3) {
			return false
		}
		for _, r := range group {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// stderrLineFilter removes provider-specific noise from the live view while preserving every
// other stderr byte, including a final line without a newline.
type stderrLineFilter struct {
	out      io.Writer
	buf      []byte
	dropping bool
	drop     func([]byte) bool
}

func newGeminiStderrFilter(out io.Writer) *stderrLineFilter {
	return &stderrLineFilter{
		out: out,
		drop: func(line []byte) bool {
			text := string(line)
			return text == geminiColorWarning || text == geminiYOLOWarning
		},
	}
}

func newCodexStderrFilter(out io.Writer) *stderrLineFilter {
	return &stderrLineFilter{
		out: out,
		drop: func(line []byte) bool {
			if bytes.Contains(line, []byte(codexRouterError)) {
				return true
			}
			return string(bytes.TrimRight(line, "\r")) == codexStdinBanner
		},
	}
}

func (f *stderrLineFilter) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		if f.dropping {
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				return written, nil
			}
			f.dropping = false
			p = p[i+1:]
			continue
		}
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if err := f.appendFragment(p); err != nil {
				return 0, err
			}
			break
		}
		if err := f.appendFragment(p[:i]); err != nil {
			return 0, err
		}
		if !f.dropping {
			if err := f.writeLine(f.buf, true); err != nil {
				return 0, err
			}
		}
		f.buf = nil
		f.dropping = false
		p = p[i+1:]
	}
	return written, nil
}

func (f *stderrLineFilter) appendFragment(p []byte) error {
	remaining := maxStreamEventBytes - len(f.buf)
	if len(p) <= remaining {
		f.buf = append(f.buf, p...)
		return nil
	}
	f.buf = append(f.buf, p[:remaining]...)
	if _, err := f.out.Write(f.buf); err != nil {
		return err
	}
	if _, err := io.WriteString(f.out, " [truncated]\n"); err != nil {
		return err
	}
	f.buf = nil
	f.dropping = true
	return nil
}

func (f *stderrLineFilter) flush() error {
	if len(f.buf) == 0 {
		return nil
	}
	err := f.writeLine(f.buf, false)
	f.buf = nil
	return err
}

func (f *stderrLineFilter) writeLine(line []byte, newline bool) error {
	if f.drop(line) {
		return nil
	}
	if _, err := f.out.Write(line); err != nil {
		return err
	}
	if newline {
		_, err := io.WriteString(f.out, "\n")
		return err
	}
	return nil
}

// codexStreamDecoder renders `codex exec --json` events into the loop's common activity view.
type codexStreamDecoder struct {
	*ndjsonDecoder
	agent   string
	profile string
	root    string
	model   string
	tool    boundedLabels
	shown   boundedLabels
	last    *iterResult
	failed  bool
}

func newCodexStreamDecoder(out, tail io.Writer, agent, profile, root, model string) *codexStreamDecoder {
	d := &codexStreamDecoder{agent: agent, profile: profile, root: root, model: model}
	d.ndjsonDecoder = newNDJSONDecoder(out, tail, d.event)
	return d
}

func (d *codexStreamDecoder) event(raw json.RawMessage) {
	var ev codexStreamEvent
	if json.Unmarshal(raw, &ev) != nil {
		d.passthrough(raw)
		return
	}
	switch ev.Type {
	case "thread.started":
		d.noteBootstrap()
		d.showModel()
	case "turn.started":
		// The turn was dispatched, but nothing proves the model acted yet.
	case "item.started":
		d.noteItemActivity(ev.Item, true)
		d.itemStarted(ev.Item)
	case "item.updated":
		if codexActivityItem(ev.Item) {
			d.noteProgress()
		}
	case "item.completed":
		d.noteItemActivity(ev.Item, false)
		d.itemCompleted(ev.Item)
	case "turn.completed":
		d.noteTerminal()
		d.last = &iterResult{
			InTok:  ev.Usage.InputTokens,
			OutTok: ev.Usage.OutputTokens + ev.Usage.ReasoningOutputTokens,
		}
		d.emit(ui.Dim("· " + tokenUsage(d.last.InTok, d.last.OutTok)))
	case "turn.failed":
		d.noteTerminal()
		d.failed = true
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = strings.TrimSpace(ev.Error.Message)
		}
		if msg == "" {
			msg = "turn failed"
		}
		d.emit(d.streamErrorLine(msg))
		d.toTail(msg)
		d.toDiagnostic(msg)
	default:
		d.emitUnknown(ev.Type)
	}
}

func (d *codexStreamDecoder) showModel() {
	d.emit(streamModelLine(d.agent, streamDisplayModel(d.model), d.profile))
}

// codexRecognizedItem lists the item types the Codex stream is known to emit; only these prove
// model progress for the watchdog. Unknown item types render but never reset activity.
func codexRecognizedItem(kind string) bool {
	switch kind {
	case "agent_message", "reasoning", "command_execution", "file_change",
		"mcp_tool_call", "web_search", "collab_tool_call", "todo_list":
		return true
	}
	return false
}

// codexBlockingItem marks the item kinds that run a foreground child whose lifecycle the
// watchdog tracks by ID — a gate under command_execution, an MCP call, a collab wait — so the
// idle deadline suspends until their completion while the absolute tool cap still bounds them.
func codexBlockingItem(kind string) bool {
	switch kind {
	case "command_execution", "mcp_tool_call", "collab_tool_call":
		return true
	}
	return false
}

// codexActivityItem reports whether one item is semantic activity: a recognized kind AND the id
// the codex schema keys every lifecycle event on. An id-less item is not a half-event to be
// generous about — `{"item":{"type":"reasoning"}}` is the empty envelope an untrusted stream would
// use for a free deadline reset, and an id-less start could never be paired with the completion
// that resumes the idle deadline it suspended.
func codexActivityItem(item codexStreamItem) bool {
	return codexRecognizedItem(item.Type) && item.ID != ""
}

func (d *codexStreamDecoder) noteItemActivity(item codexStreamItem, started bool) {
	if !codexActivityItem(item) {
		return
	}
	if codexBlockingItem(item.Type) {
		if started {
			d.noteToolStart(item.ID)
		} else {
			d.noteToolEnd(item.ID)
		}
		return
	}
	d.noteProgress()
}

func (d *codexStreamDecoder) itemStarted(item codexStreamItem) {
	switch item.Type {
	case "command_execution":
		label := streamCommandLabel(item.Command)
		d.emit(d.streamToolLine("⚙", label, false))
		d.tool.set(item.ID, label)
	case "web_search":
		d.showItem(item, "⌕", codexWebSearchLabel(item.Query))
	case "collab_tool_call":
		d.showItem(item, "⇢", codexCollabLabel(item.Tool))
	case "todo_list":
	}
}

func (d *codexStreamDecoder) itemCompleted(item codexStreamItem) {
	switch item.Type {
	case "agent_message":
		if text := strings.TrimSpace(item.Text); text != "" {
			d.emit(ui.Magenta(llmIcon) + " " + text)
			d.toTail(text)
		}
	case "command_execution":
		label := d.tool.take(item.ID)
		if item.ExitCode == nil || *item.ExitCode == 0 {
			return
		}
		if label == "" {
			label = streamCommandLabel(item.Command)
		}
		d.emit(d.streamFailureLine(label, fmt.Sprintf(" (exit %d)", *item.ExitCode), commandFailureDiagnostic(item.AggregatedOutput), streamToolTextWidth))
	case "file_change":
		d.fileChange(item)
	case "web_search":
		d.showItem(item, "⌕", codexWebSearchLabel(item.Query))
	case "collab_tool_call":
		d.showItem(item, "⇢", codexCollabLabel(item.Tool))
	case "todo_list":
	default:
		d.emitUnknown(item.Type)
	}
}

func (d *codexStreamDecoder) fileChange(item codexStreamItem) {
	shown := false
	for _, change := range item.Changes {
		path := strings.TrimSpace(change.Path)
		if path == "" {
			continue
		}
		label, inside := repoRel(d.root, path)
		d.emit(d.streamToolLine("✎", label, !inside))
		shown = true
	}
	if !shown {
		d.emit(d.streamToolLine("✎", "file change", false))
	}
}

func (d *codexStreamDecoder) showItem(item codexStreamItem, glyph, label string) {
	if !d.shown.mark(item.Type + "\x00" + item.ID) {
		return
	}
	d.emit(d.streamToolLine(glyph, label, false))
}

func codexWebSearchLabel(query string) string {
	if query = strings.TrimSpace(query); query != "" {
		return query
	}
	return "web search"
}

func codexCollabLabel(tool string) string {
	tool = strings.TrimSpace(tool)
	switch tool {
	case "spawn_agent":
		return "spawn agent"
	case "send_input":
		return "send input"
	case "wait":
		return "wait"
	case "close_agent":
		return "close agent"
	case "":
		return "collaboration"
	default:
		return strings.Join(strings.Fields(strings.ReplaceAll(tool, "_", " ")), " ")
	}
}

func (d *codexStreamDecoder) emitUnknown(kind string) {
	if kind == "" {
		kind = "unknown"
	}
	d.emit(ui.Dim("· " + kind))
}

func (d *codexStreamDecoder) lastIterResult() *iterResult { return d.last }
func (d *codexStreamDecoder) streamOutcome() providerStreamOutcome {
	if d.malformed {
		return streamMalformed
	}
	if d.failed {
		return streamFailed
	}
	if d.last != nil {
		return streamSucceeded
	}
	return streamIncomplete
}

func streamCommandLabel(command string) string {
	command = strings.TrimSpace(command)
	command = strings.TrimPrefix(command, "/bin/bash -lc ")
	return firstLine(stripLeadingCD(command))
}

type codexStreamEvent struct {
	Type    string          `json:"type"`
	Item    codexStreamItem `json:"item"`
	Message string          `json:"message"`
	Error   struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage codexStreamUsage `json:"usage"`
}

type codexStreamItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	Text             string `json:"text"`
	Query            string `json:"query"`
	Tool             string `json:"tool"`
	Changes          []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
}

type codexStreamUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// geminiStreamDecoder renders `gemini -o stream-json` events. Gemini emits assistant text as
// deltas, so the decoder holds one narration line until the next non-assistant event.
type geminiStreamDecoder struct {
	*ndjsonDecoder
	agent     string
	profile   string
	root      string
	model     string
	assistant boundedNarration
	tool      boundedLabels
	last      *iterResult
	failed    bool
}

func newGeminiStreamDecoder(out, tail io.Writer, agent, profile, root, model string) *geminiStreamDecoder {
	d := &geminiStreamDecoder{agent: agent, profile: profile, root: root, model: model}
	d.ndjsonDecoder = newNDJSONDecoder(out, tail, d.event)
	d.ndjsonDecoder.beforeRaw = d.flushAssistant
	return d
}

func (d *geminiStreamDecoder) event(raw json.RawMessage) {
	var ev geminiStreamEvent
	if json.Unmarshal(raw, &ev) != nil {
		d.passthrough(raw)
		return
	}
	if ev.Type != "message" || ev.Role != "assistant" {
		d.flushAssistant()
	}
	switch ev.Type {
	case "init":
		d.noteBootstrap()
		d.emit(streamModelLine(d.agent, streamDisplayModel(d.model), d.profile))
	case "message":
		switch ev.Role {
		case "assistant":
			// Gemini streams narration as deltas, so the delta's own text is the whole proof of
			// model action: a contentless assistant message is an empty envelope, not a turn.
			if ev.Content != "" {
				d.noteProgress()
			}
			d.assistant.WriteString(ev.Content)
		case "user":
			// Gemini echoes the whole prompt as a user message; it is intentionally suppressed —
			// and being the host's own prompt, it proves nothing about model progress either.
		default:
			role := strings.TrimSpace("message " + ev.Role)
			d.emit(ui.Dim("· " + role))
		}
	case "tool_use":
		// tool_id is the handle the matching tool_result arrives under; without it the pair cannot
		// be closed, so the event is shown but suspends no deadline.
		if ev.ToolID != "" {
			d.noteToolStart(ev.ToolID)
		}
		d.toolUse(&ev)
	case "tool_result":
		if ev.ToolID != "" {
			d.noteToolEnd(ev.ToolID)
		}
		d.toolResult(&ev)
	case "result":
		d.noteTerminal()
		d.result(&ev)
	case "error":
		d.noteTerminal()
		d.failed = true
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = jsonEventMessage(ev.Error)
		}
		if msg == "" {
			msg = "error"
		}
		d.emit(d.streamErrorLine(msg))
		d.toTail(msg)
		d.toDiagnostic(msg)
	default:
		d.emitUnknown(ev.Type)
	}
}

func (d *geminiStreamDecoder) flush() {
	d.ndjsonDecoder.flush()
	d.flushAssistant()
}

func (d *geminiStreamDecoder) flushAssistant() {
	text := strings.TrimSpace(d.assistant.Take())
	if text == "" {
		return
	}
	d.emit(ui.Magenta(llmIcon) + " " + text)
	d.toTail(text)
}

func (d *geminiStreamDecoder) toolUse(ev *geminiStreamEvent) {
	name := ev.ToolName
	label := ""
	line := ""
	switch name {
	case "read_file", "read_many_files":
		label, line = d.fileToolLine("▸", ev.Parameters.FilePath)
	case "write_file", "replace", "edit":
		label, line = d.fileToolLine("✎", ev.Parameters.FilePath)
	case "run_shell_command":
		label = firstLine(stripLeadingCD(ev.Parameters.Command))
		line = d.streamToolLine("⚙", label, false)
	default:
		label = ev.Parameters.Description
		line = ui.Dim("· " + strings.TrimSpace(name+" "+label))
	}
	d.emit(line)
	d.tool.set(ev.ToolID, strings.TrimSpace(name+" "+label))
}

func (d *geminiStreamDecoder) fileToolLine(glyph, path string) (label, line string) {
	label, inside := repoRel(d.root, path)
	return label, d.streamToolLine(glyph, label, !inside)
}

func (d *geminiStreamDecoder) toolResult(ev *geminiStreamEvent) {
	label := d.tool.take(ev.ToolID)
	if ev.Status == "success" {
		return
	}
	output := ev.Output
	if output == "" {
		output = jsonEventMessage(ev.Error)
	}
	d.emit(d.streamFailureLine(label, "", firstLine(output), 0))
}

func (d *geminiStreamDecoder) result(ev *geminiStreamEvent) {
	d.last = &iterResult{
		DurationMS: ev.Stats.DurationMS,
		InTok:      ev.Stats.InputTokens,
		OutTok:     ev.Stats.OutputTokens,
	}
	dur := (time.Duration(ev.Stats.DurationMS) * time.Millisecond).Round(time.Second)
	d.emit(ui.Dim(fmt.Sprintf("· %s · %s", dur, tokenUsage(d.last.InTok, d.last.OutTok))))
	if ev.Status == "success" {
		return
	}
	d.failed = true
	msg := strings.TrimSpace(ev.Message)
	if msg == "" {
		msg = jsonEventMessage(ev.Error)
	}
	if msg == "" {
		msg = ev.Status
	}
	d.emit(d.streamErrorLine(msg))
	d.toTail(msg)
	d.toDiagnostic(msg)
}

func (d *geminiStreamDecoder) emitUnknown(kind string) {
	if kind == "" {
		kind = "unknown"
	}
	d.emit(ui.Dim("· " + kind))
}

func (d *geminiStreamDecoder) lastIterResult() *iterResult { return d.last }
func (d *geminiStreamDecoder) streamOutcome() providerStreamOutcome {
	if d.malformed {
		return streamMalformed
	}
	if d.failed {
		return streamFailed
	}
	if d.last != nil {
		return streamSucceeded
	}
	return streamIncomplete
}

type geminiStreamEvent struct {
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolName   string          `json:"tool_name"`
	ToolID     string          `json:"tool_id"`
	Status     string          `json:"status"`
	Output     string          `json:"output"`
	Message    string          `json:"message"`
	Error      json.RawMessage `json:"error"`
	Parameters struct {
		FilePath    string `json:"file_path"`
		Command     string `json:"command"`
		Description string `json:"description"`
	} `json:"parameters"`
	Stats struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		DurationMS   int `json:"duration_ms"`
	} `json:"stats"`
}

// grokStreamDecoder renders Grok's streaming-json deltas. Thought tokens stay hidden; text
// deltas are coalesced into the same narration line used by the other providers.
type grokStreamDecoder struct {
	*ndjsonDecoder
	agent      string
	profile    string
	model      string
	modelShown bool
	text       boundedNarration
	last       *iterResult
	failed     bool
}

func newGrokStreamDecoder(out, tail io.Writer, agent, profile, _ string, model string) *grokStreamDecoder {
	d := &grokStreamDecoder{agent: agent, profile: profile, model: model}
	d.ndjsonDecoder = newNDJSONDecoder(out, tail, d.event)
	d.ndjsonDecoder.beforeRaw = d.flushText
	return d
}

func (d *grokStreamDecoder) event(raw json.RawMessage) {
	var ev grokStreamEvent
	if json.Unmarshal(raw, &ev) != nil {
		d.passthrough(raw)
		return
	}
	d.showModel()
	if ev.Type != "text" {
		d.flushText()
	}
	switch ev.Type {
	case "thought":
		// Grok streams both narration and reasoning as deltas carried in `data`, so an empty delta
		// is an empty envelope: hidden reasoning is still model action, but only if there is any.
		if ev.Data != "" {
			d.noteProgress()
		}
	case "text":
		if ev.Data != "" {
			d.noteProgress()
		}
		d.text.WriteString(ev.Data)
	case "end":
		d.noteTerminal()
		d.last = &iterResult{
			Turns:  ev.NumTurns,
			InTok:  ev.Usage.InputTokens + ev.Usage.CacheReadInputTokens,
			OutTok: ev.Usage.OutputTokens + ev.Usage.ReasoningTokens,
		}
		d.emit(ui.Dim(fmt.Sprintf("· %d turns · %s", d.last.Turns, tokenUsage(d.last.InTok, d.last.OutTok))))
	default:
		if strings.Contains(strings.ToLower(ev.Type), "error") {
			d.failed = true
			d.passthrough(raw)
			return
		}
		kind := ev.Type
		if kind == "" {
			kind = "unknown"
		}
		d.emit(ui.Dim("· " + kind))
	}
}

func (d *grokStreamDecoder) flush() {
	d.ndjsonDecoder.flush()
	d.flushText()
}

func (d *grokStreamDecoder) showModel() {
	if d.modelShown {
		return
	}
	d.modelShown = true
	d.emit(streamModelLine(d.agent, streamDisplayModel(d.model), d.profile))
}

func (d *grokStreamDecoder) flushText() {
	text := strings.TrimSpace(d.text.Take())
	if text == "" {
		return
	}
	d.emit(ui.Magenta(llmIcon) + " " + text)
	d.toTail(text)
}

func (d *grokStreamDecoder) lastIterResult() *iterResult { return d.last }
func (d *grokStreamDecoder) streamOutcome() providerStreamOutcome {
	if d.malformed {
		return streamMalformed
	}
	if d.failed {
		return streamFailed
	}
	if d.last != nil {
		return streamSucceeded
	}
	return streamIncomplete
}

type grokStreamEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	NumTurns int    `json:"num_turns"`
	Usage    struct {
		InputTokens          int `json:"input_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		ReasoningTokens      int `json:"reasoning_tokens"`
	} `json:"usage"`
}

func jsonEventMessage(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.Message)
	}
	return ""
}

func streamDisplayModel(model string) string {
	if model == "" {
		return "default"
	}
	return model
}
