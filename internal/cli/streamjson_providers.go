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

const (
	geminiColorWarning = "Warning: 256-color support not detected. Using a terminal with at least 256-color support is recommended for a better visual experience."
	geminiYOLOWarning  = "YOLO mode is enabled. All tool calls will be automatically approved."
)

// geminiStderrFilter removes two unconditional CLI notices from the live view while preserving
// every other stderr byte, including a final line without a newline.
type geminiStderrFilter struct {
	out io.Writer
	buf []byte
}

func newGeminiStderrFilter(out io.Writer) *geminiStderrFilter {
	return &geminiStderrFilter{out: out}
}

func (f *geminiStderrFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		if err := f.writeLine(f.buf[:i], true); err != nil {
			return 0, err
		}
		f.buf = f.buf[i+1:]
	}
	return len(p), nil
}

func (f *geminiStderrFilter) flush() error {
	if len(f.buf) == 0 {
		return nil
	}
	err := f.writeLine(f.buf, false)
	f.buf = nil
	return err
}

func (f *geminiStderrFilter) writeLine(line []byte, newline bool) error {
	if string(line) == geminiColorWarning || string(line) == geminiYOLOWarning {
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
	tool    map[string]string
	last    *iterResult
}

func newCodexStreamDecoder(out, tail io.Writer, agent, profile, root, model string) *codexStreamDecoder {
	d := &codexStreamDecoder{
		agent: agent, profile: profile, root: root, model: model,
		tool: map[string]string{},
	}
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
		d.showModel()
	case "turn.started":
	case "item.started":
		d.itemStarted(ev.Item)
	case "item.completed":
		d.itemCompleted(ev.Item)
	case "turn.completed":
		d.last = &iterResult{
			InTok:  ev.Usage.InputTokens,
			OutTok: ev.Usage.OutputTokens + ev.Usage.ReasoningOutputTokens,
		}
		d.emit(ui.Dim(fmt.Sprintf("· %s/%s tok", humanTokens(d.last.InTok), humanTokens(d.last.OutTok))))
	case "turn.failed":
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = strings.TrimSpace(ev.Error.Message)
		}
		if msg == "" {
			msg = "turn failed"
		}
		d.emit(ui.Red("✗ " + truncate(firstLine(msg), 80)))
		d.toTail(msg)
	default:
		d.emitUnknown(ev.Type)
	}
}

func (d *codexStreamDecoder) showModel() {
	d.emit(streamModelLine(d.agent, streamDisplayModel(d.model), d.profile))
}

func (d *codexStreamDecoder) itemStarted(item codexStreamItem) {
	if item.Type != "command_execution" {
		return
	}
	label := streamCommandLabel(item.Command)
	shown := truncate(label, 60)
	d.emit(streamToolLine("⚙", shown, false))
	d.tool[item.ID] = shown
}

func (d *codexStreamDecoder) itemCompleted(item codexStreamItem) {
	switch item.Type {
	case "agent_message":
		if text := strings.TrimSpace(item.Text); text != "" {
			d.emit(ui.Magenta(llmIcon) + " " + text)
			d.toTail(text)
		}
	case "command_execution":
		defer delete(d.tool, item.ID)
		if item.ExitCode == nil || *item.ExitCode == 0 {
			return
		}
		label := d.tool[item.ID]
		if label == "" {
			label = truncate(streamCommandLabel(item.Command), 60)
		}
		line := "  " + ui.Red("✗")
		if label != "" {
			line += " " + label
		}
		if first := firstLine(item.AggregatedOutput); first != "" {
			line += ": " + truncate(first, 60)
		}
		d.emit(line)
	default:
		d.emitUnknown(item.Type)
	}
}

func (d *codexStreamDecoder) emitUnknown(kind string) {
	if kind == "" {
		kind = "unknown"
	}
	d.emit(ui.Dim("· " + kind))
}

func (d *codexStreamDecoder) lastIterResult() *iterResult { return d.last }

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
	assistant strings.Builder
	tool      map[string]string
	last      *iterResult
}

func newGeminiStreamDecoder(out, tail io.Writer, agent, profile, root, model string) *geminiStreamDecoder {
	d := &geminiStreamDecoder{
		agent: agent, profile: profile, root: root, model: model,
		tool: map[string]string{},
	}
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
		d.emit(streamModelLine(d.agent, streamDisplayModel(d.model), d.profile))
	case "message":
		switch ev.Role {
		case "assistant":
			d.assistant.WriteString(ev.Content)
		case "user":
			// Gemini echoes the whole prompt as a user message; it is intentionally suppressed.
		default:
			role := strings.TrimSpace("message " + ev.Role)
			d.emit(ui.Dim("· " + role))
		}
	case "tool_use":
		d.toolUse(&ev)
	case "tool_result":
		d.toolResult(&ev)
	case "result":
		d.result(&ev)
	case "error":
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = jsonEventMessage(ev.Error)
		}
		if msg == "" {
			msg = "error"
		}
		d.emit(ui.Red("✗ " + truncate(firstLine(msg), 80)))
		d.toTail(msg)
	default:
		d.emitUnknown(ev.Type)
	}
}

func (d *geminiStreamDecoder) flush() {
	d.ndjsonDecoder.flush()
	d.flushAssistant()
}

func (d *geminiStreamDecoder) flushAssistant() {
	text := strings.TrimSpace(d.assistant.String())
	d.assistant.Reset()
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
		line = streamToolLine("⚙", label, false)
	default:
		label = ev.Parameters.Description
		line = ui.Dim("· " + strings.TrimSpace(name+" "+label))
	}
	d.emit(line)
	d.tool[ev.ToolID] = strings.TrimSpace(name + " " + label)
}

func (d *geminiStreamDecoder) fileToolLine(glyph, path string) (label, line string) {
	label, inside := repoRel(d.root, path)
	return label, streamToolLine(glyph, label, !inside)
}

func (d *geminiStreamDecoder) toolResult(ev *geminiStreamEvent) {
	defer delete(d.tool, ev.ToolID)
	if ev.Status == "success" {
		return
	}
	line := "  " + ui.Red("✗")
	if label := d.tool[ev.ToolID]; label != "" {
		line += " " + label
	}
	output := ev.Output
	if output == "" {
		output = jsonEventMessage(ev.Error)
	}
	if first := firstLine(output); first != "" {
		line += ": " + truncate(first, 60)
	}
	d.emit(line)
}

func (d *geminiStreamDecoder) result(ev *geminiStreamEvent) {
	d.last = &iterResult{
		DurationMS: ev.Stats.DurationMS,
		InTok:      ev.Stats.InputTokens,
		OutTok:     ev.Stats.OutputTokens,
	}
	dur := (time.Duration(ev.Stats.DurationMS) * time.Millisecond).Round(time.Second)
	d.emit(ui.Dim(fmt.Sprintf("· %s · %s/%s tok", dur, humanTokens(d.last.InTok), humanTokens(d.last.OutTok))))
	if ev.Status == "success" {
		return
	}
	msg := strings.TrimSpace(ev.Message)
	if msg == "" {
		msg = jsonEventMessage(ev.Error)
	}
	if msg == "" {
		msg = ev.Status
	}
	d.emit(ui.Red("✗ " + truncate(firstLine(msg), 80)))
	d.toTail(msg)
}

func (d *geminiStreamDecoder) emitUnknown(kind string) {
	if kind == "" {
		kind = "unknown"
	}
	d.emit(ui.Dim("· " + kind))
}

func (d *geminiStreamDecoder) lastIterResult() *iterResult { return d.last }

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
	text       strings.Builder
	last       *iterResult
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
	case "text":
		d.text.WriteString(ev.Data)
	case "end":
		d.last = &iterResult{
			Turns:  ev.NumTurns,
			InTok:  ev.Usage.InputTokens + ev.Usage.CacheReadInputTokens,
			OutTok: ev.Usage.OutputTokens + ev.Usage.ReasoningTokens,
		}
		d.emit(ui.Dim(fmt.Sprintf("· %d turns · %s/%s tok", d.last.Turns, humanTokens(d.last.InTok), humanTokens(d.last.OutTok))))
	default:
		if strings.Contains(strings.ToLower(ev.Type), "error") {
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
	text := strings.TrimSpace(d.text.String())
	d.text.Reset()
	if text == "" {
		return
	}
	d.emit(ui.Magenta(llmIcon) + " " + text)
	d.toTail(text)
}

func (d *grokStreamDecoder) lastIterResult() *iterResult { return d.last }

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

func streamToolLine(glyph, label string, outside bool) string {
	line := glyph
	if label == "" {
		return line
	}
	shown := truncate(label, 60)
	if outside {
		return line + " " + ui.Yellow("⚠ "+shown)
	}
	return line + " " + ui.Dim(shown)
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
