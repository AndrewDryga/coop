package acpproxy

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// maxStreams bounds what the guard remembers, because a box chooses every id it sends. A stream it could
// not remember starts guarded, so going past this costs a backslash and opens nothing.
const maxStreams = 4096

// maxStreamID keeps long ids from filling that memory. An adapter's are short, and a chunk with a longer
// one is still escaped, only without help from the chunks around it.
const maxStreamID = 1 << 10

// notMarkdown names what reaches a diff view or a terminal, where a backslash would only be wrong.
var notMarkdown = map[string]bool{"oldText": true, "newText": true, "terminal_output": true}

// imageGuard is what a filtered session writes to the editor through, so that no line can carry an image
// the editor would load, whoever built it: Coop's own lines hold a box's words too, as in a failed setting's error. Each
// text gets the escaper's rules. A session's text chunks are one stream, so it is the guard that holds
// back what they leave undecided, and sends that before anything else the session says.
type imageGuard struct {
	mu      sync.Mutex
	editor  io.Writer
	open    map[string]*textStream // the stream each session wrote to last
	exposed map[streamID]bool      // streams no longer open that ended on a raw `!` or `]`
	full    bool                   // exposed ran out of room, so any stream may be one of those
}

// streamID names the text the editor joins into one Markdown.
type streamID struct{ session, kind, id string }

type textStream struct {
	streamID
	idKey   string // the member the id came from
	wrapped bool   // a tool's chunk, which holds its block one level down
	text    imageEscaper
}

func newImageGuard(editor io.Writer) *imageGuard {
	return &imageGuard{editor: editor, open: map[string]*textStream{}, exposed: map[streamID]bool{}}
}

func (g *imageGuard) Write(lines []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []byte
	for rest := lines; len(rest) > 0; {
		end := bytes.IndexByte(rest, '\n') + 1
		if end == 0 {
			end = len(rest)
		}
		out = append(out, g.rewrite(rest[:end])...)
		rest = rest[end:]
	}
	if len(out) > 0 {
		if _, err := g.editor.Write(out); err != nil {
			return 0, err
		}
	}
	return len(lines), nil
}

// flush sends what every stream holds, for when no line is coming to carry it: the child is gone.
func (g *imageGuard) flush() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if out := g.pauseAll(); len(out) > 0 {
		_, _ = g.editor.Write(out)
	}
}

// sendHeldText is imageGuard.flush for a session that has one.
func (p *proxy) sendHeldText() {
	if guard, filtered := p.out.(*imageGuard); filtered {
		guard.flush()
	}
}

// rewrite returns what the editor should get for this line, with anything held that must go first. A
// line that says nothing about images goes out byte for byte. One that does not parse does too: the
// proxy has refused any such line from a box, and no editor could read a field from it either.
func (g *imageGuard) rewrite(line []byte) []byte {
	if !json.Valid(line) {
		return line
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	value, plain := readPlainValue(decoder)
	if !plain {
		Trace("dropped a line for the editor that parsers could read differently")
		return nil
	}
	message, isObject := value.(map[string]any)
	if !isObject {
		return line
	}
	params, _ := message["params"].(map[string]any)
	session, _ := params["sessionId"].(string)
	update, _ := params["update"].(map[string]any)
	kind, _ := update["sessionUpdate"].(string)
	if content, block := update["content"].(map[string]any); block && message["method"] == "session/update" && session != "" && kind != "" {
		return g.rewriteChunk(line, message, streamID{session: session, kind: kind}, update, content)
	}
	// Any other line ends what a stream was writing, and an answer has no session to say whose.
	var out []byte
	if session != "" {
		out = g.pauseSession(session)
	} else {
		out = g.pauseAll()
	}
	var text lineText
	return append(out, text.finished(line, message)...)
}

// rewriteChunk handles one chunk of a stream: `update.content` holds a single block, not a list.
func (g *imageGuard) rewriteChunk(line []byte, message map[string]any, id streamID, update, content map[string]any) []byte {
	block, wrapped := content["content"].(map[string]any)
	if !wrapped {
		block = content
	}
	var idKey string
	for _, key := range []string{"messageId", "toolCallId", "compactionId"} {
		if named, ok := update[key].(string); ok {
			idKey, id.id = key, named
			break
		}
	}
	var stream *textStream
	var out []byte
	after := moreText
	switch {
	case len(id.session)+len(id.kind)+len(id.id) > maxStreamID:
		stream, after = &textStream{streamID: id}, sealText
		stream.text.exposed = true
		out = g.pauseSession(id.session)
	default:
		stream, out = g.streamFor(id, idKey, wrapped)
		// Zed drops a user chunk that equals what it already shows, which only works if chunks stay whole.
		if id.kind == "user_message_chunk" {
			after = pauseText
		}
	}
	text, isText := block["text"].(string)
	if !isText || block["type"] != "text" {
		// Zed writes a link into the message's Markdown, right after what came before it.
		if sent := stream.text.finish(sealText); len(sent) > 0 {
			out = append(out, stream.line(sent)...)
		}
		if stream.text.exposed && (block["type"] == "resource_link" || block["type"] == "resource") {
			out = append(out, stream.line([]byte(`\`))...)
		}
		stream.text.exposed = false
		var rest lineText
		return append(out, rest.finished(line, message)...)
	}
	delete(block, "text")
	written := stream.text.write([]byte(text))
	if after != moreText {
		written = append(written, stream.text.finish(after)...)
	}
	rest := lineText{changed: string(written) != text}
	rest.rewrite(message, false)
	if len(written) == 0 && text != "" && !rest.refused {
		return out // nothing to say yet: what this chunk held back goes out with the next one
	}
	block["text"] = string(written)
	return append(out, rest.encoded(line, message)...)
}

// streamFor finds the stream a chunk continues, as Zed joins chunks: same kind, and ids that do not
// differ. It also returns what must go out first, when this chunk ends another stream.
func (g *imageGuard) streamFor(id streamID, idKey string, wrapped bool) (*textStream, []byte) {
	current := g.open[id.session]
	if current != nil && current.kind == id.kind && (current.id == id.id || current.id == "" || id.id == "") {
		if current.id == "" {
			current.id, current.idKey = id.id, idKey
		}
		return current, nil
	}
	var out []byte
	if current != nil {
		out = g.retire(current)
	} else if len(g.open) >= maxStreams {
		for _, stream := range g.open {
			out = append(out, g.retire(stream)...)
		}
		clear(g.open)
	}
	stream := &textStream{streamID: id, idKey: idKey, wrapped: wrapped}
	// Zed gives each of a tool's chunks its own Markdown today, but the name says they stream, and the
	// line before may have ended on a raw `!` that no stream saw.
	stream.text.exposed = g.full || g.exposed[id] || idKey == "toolCallId"
	delete(g.exposed, id)
	g.open[id.session] = stream
	return stream, out
}

func (g *imageGuard) retire(stream *textStream) []byte {
	out := stream.pause()
	if stream.text.exposed {
		if len(g.exposed) < maxStreams {
			g.exposed[stream.streamID] = true
		} else {
			g.full = true
		}
	}
	return out
}

func (g *imageGuard) pauseSession(session string) []byte {
	if stream := g.open[session]; stream != nil {
		return stream.pause()
	}
	return nil
}

func (g *imageGuard) pauseAll() []byte {
	var out []byte
	for _, stream := range g.open {
		out = append(out, stream.pause()...)
	}
	return out
}

// pause sends what the stream holds as a chunk of its own, keeping what guards the next one.
func (s *textStream) pause() []byte {
	if text := s.text.finish(pauseText); len(text) > 0 {
		return s.line(text)
	}
	return nil
}

func (s *textStream) line(text []byte) []byte {
	var content any = map[string]any{"type": "text", "text": string(text)}
	if s.wrapped {
		content = map[string]any{"type": "content", "content": content}
	}
	update := map[string]any{"sessionUpdate": s.kind, "content": content}
	if s.idKey != "" {
		update[s.idKey] = s.id
	}
	return encodedLine(map[string]any{"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": s.session, "update": update}})
}

// readPlainValue decodes the next JSON value. It reports false on a repeated key at any depth: parsers
// keep the first, the last, or fail, so the text checked here might not be the text the editor shows.
func readPlainValue(decoder *json.Decoder) (any, bool) {
	token, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for decoder.More() {
			key, err := decoder.Token()
			name, isName := key.(string)
			if _, repeated := object[name]; err != nil || !isName || repeated {
				return nil, false
			}
			value, plain := readPlainValue(decoder)
			if !plain {
				return nil, false
			}
			object[name] = value
		}
		_, err = decoder.Token()
		return object, err == nil
	case json.Delim('['):
		list := []any{}
		for decoder.More() {
			value, plain := readPlainValue(decoder)
			if !plain {
				return nil, false
			}
			list = append(list, value)
		}
		_, err = decoder.Token()
		return list, err == nil
	}
	return token, true
}

func encodedLine(message map[string]any) []byte {
	var line bytes.Buffer
	encoder := json.NewEncoder(&line)
	encoder.SetEscapeHTML(false) // this text is read back as text, and each change of it is one to explain
	if encoder.Encode(message) != nil {
		return nil
	}
	return line.Bytes()
}

// lineText applies the rules to every string in a line, so a field nobody listed is covered too. Each is
// complete on its own: Zed replaces a title or a plan entry, it does not add to it.
type lineText struct{ changed, refused bool }

func (t *lineText) finished(line []byte, message map[string]any) []byte {
	t.rewrite(message, false)
	return t.encoded(line, message)
}

func (t *lineText) encoded(line []byte, message map[string]any) []byte {
	switch {
	case t.refused:
		Trace("dropped a line for the editor: a link in it spells an image")
		return nil
	case t.changed:
		Trace("escaped a line for the editor, which would have loaded an image")
		return encodedLine(message)
	}
	return line
}

func (t *lineText) rewrite(value any, inList bool) any {
	switch value := value.(type) {
	case string:
		return t.escape(value, lastText)
	case []any:
		for i, item := range value {
			value[i] = t.rewrite(item, true)
		}
	case map[string]any:
		for key, member := range value {
			text, isString := member.(string)
			switch {
			case notMarkdown[key]:
			case (key == "rawInput" || key == "rawOutput") && !isString:
				// Zed prints this as JSON in a code fence, and JSON has no line break that could close it.
			case key == "uri" && isString:
				t.refused = t.refused || spellsImage(text)
			case key == "text" && isString && inList && value["type"] == "text":
				// Zed joins the text blocks of a message into one Markdown, and a chunk may follow the last.
				value[key] = t.escape(text, sealText)
			default:
				value[key] = t.rewrite(member, false)
			}
		}
	}
	return value
}

func (t *lineText) escape(text string, end textEnd) string {
	escaped := escapeWhole(text, end)
	t.changed = t.changed || escaped != text
	return escaped
}

// spellsImage reports a link no backslash can fix. Zed writes a resource link into a message as
// `[@name](uri)`, or as the bare uri if it cannot read it, and takes the name from the uri with its %
// escapes decoded. The next chunk joins right after it, so a tail that chunk could finish counts too.
// Only a hostile program, or a file with a very odd name, gets here.
func spellsImage(uri string) bool {
	var decoded strings.Builder
	for i := 0; i < len(uri); i++ {
		if uri[i] == '%' && i+2 < len(uri) {
			if value, err := hex.DecodeString(uri[i+1 : i+3]); err == nil {
				decoded.WriteByte(value[0])
				i += 2
				continue
			}
		}
		decoded.WriteByte(uri[i])
	}
	text := decoded.String()
	// `[@a]: b](uri)` defines `@a` with a local address, so a `]:` counts wherever it stands in a name.
	return strings.Contains(text, "![") || strings.Contains(text, "]:") || escapeWhole(text, sealText) != text
}
