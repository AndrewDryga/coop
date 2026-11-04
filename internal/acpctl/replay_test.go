package acpctl

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func userChunk(session, text string) []byte {
	return sessionChunk(session, "user_message_chunk", text)
}

func sessionChunk(session, kind, text string) []byte {
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
		"sessionId": session,
		"update":    map[string]any{"sessionUpdate": kind, "messageId": "m1", "content": map[string]any{"type": "text", "text": text}},
	}})
	return append(line, '\n')
}

func reopen(t *testing.T, c *Control, id int, method, session string) []byte {
	t.Helper()
	_, answer := reopenHearing(t, c, id, method, session)
	return answer
}

// reopenHearing also returns what the thread hears before the request goes out.
func reopenHearing(t *testing.T, c *Control, id int, method, session string) (opening, answer []byte) {
	t.Helper()
	request := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{"sessionId":%q,"cwd":"/w","mcpServers":[]}}`+"\n", id, method, session)
	handled, opening, _, _ := c.fromEditor([]byte(request))
	if handled {
		t.Fatalf("%s was handled by the control layer", method)
	}
	return opening, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"configOptions":[]}}`+"\n", id))
}

// wireTexts decodes a toEditor result into one "kind: text" per frame, an answer as "answer".
func wireTexts(t *testing.T, out []byte) []string {
	t.Helper()
	var got []string
	for _, line := range strings.SplitAfter(string(out), "\n") {
		if line == "" {
			continue
		}
		var m struct {
			Method string `json:"method"`
			Params struct {
				Update struct {
					Kind    string `json:"sessionUpdate"`
					Content struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not one JSON frame per line: %v: %q", err, line)
		}
		if m.Method == "" {
			got = append(got, "answer")
			continue
		}
		got = append(got, m.Params.Update.Kind+": "+m.Params.Update.Content.Text)
	}
	return got
}

// A block from the real builder, as a new coop acp reads it back with no memory of injecting it.
func builtBlock(t *testing.T, entries ...string) string {
	t.Helper()
	c := newTestControl(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, text := range entries {
		c.appendHistoryLocked("built", "codex", "user", text)
	}
	return c.preambleLocked("built", false)
}

func TestReplayHidesTheCarriedBlockHoweverItIsChunked(t *testing.T) {
	block := builtBlock(t, "earlier question — with “wide” runes")
	for size := 1; size < len(block); size += 1 + size/3 {
		for _, join := range []string{"", "\n\n"} {
			c := newTestControl(t)
			answer := reopen(t, c, 7, "session/load", "S")
			text := []rune(block + join + "hi there")
			var said []string
			for start := 0; start < len(text); start += size {
				end := min(start+size, len(text))
				said = append(said, wireTexts(t, toEd(c, userChunk("S", string(text[start:end]))))...)
			}
			said = append(said, wireTexts(t, toEd(c, sessionChunk("S", "agent_message_chunk", "ok")))...)
			said = append(said, wireTexts(t, toEd(c, answer))...)
			joined := strings.Join(said, "|")
			if strings.Contains(joined, "[coop]") || strings.Contains(joined, "carried context") {
				t.Fatalf("size %d join %q: block leaked: %q", size, join, joined)
			}
			if got := strings.ReplaceAll(strings.Join(said, ""), "user_message_chunk: ", ""); got != "hi thereagent_message_chunk: okanswer" {
				t.Fatalf("size %d join %q: the thread read %q", size, join, said)
			}
		}
	}
}

// Everything but the block's text survives on the chunk that carried the person's words.
func TestReplayKeepsTheChunksOtherFields(t *testing.T) {
	c := newTestControl(t)
	reopen(t, c, 7, "session/load", "S")
	out := toEd(c, userChunk("S", builtBlock(t, "q")+"---hi"))
	if !strings.Contains(string(out), `"text":"---hi"`) || !strings.Contains(string(out), `"messageId":"m1"`) || !strings.Contains(string(out), `"sessionId":"S"`) {
		t.Fatalf("rewritten chunk: %s", out)
	}
}

// Text that only resembles the block is the person's, byte for byte and in order.
func TestReplayLeavesWhatAPersonTyped(t *testing.T) {
	whole := builtBlock(t, "q")
	end := strings.Index(whole, carriedBlockEnd)
	for name, chunks := range map[string][]string{
		"start marker alone":     {carriedBlockStart + "codex"},
		"a prefix of the marker": {"[coop] This"},
		"one bracket":            {"["},
		"text before the block":  {"see: " + whole + "hi"},
		"block in a later chunk": {"see: ", whole + "hi"},
		"no end marker":          {whole[:end]},
		"other intro":            {carriedBlockStart + "codex and more.\n\n--- conversation so far ---\nq\n" + carriedBlockEnd},
		"line break in origin":   {carriedBlockStart + "co\ndex" + carriedBlockIntro + carriedBlockEnd},
		"end marker alone":       {carriedBlockEnd + " hi"},
	} {
		for _, boundary := range []struct {
			name string
			line []byte
		}{
			{"agent", sessionChunk("S", "agent_message_chunk", "ok")},
			{"answer", []byte(`{"jsonrpc":"2.0","id":7,"result":{}}` + "\n")},
		} {
			t.Run(name+" until "+boundary.name, func(t *testing.T) {
				c := newTestControl(t)
				reopen(t, c, 7, "session/load", "S")
				var out []byte
				var want []byte
				for _, chunk := range chunks {
					want = append(want, userChunk("S", chunk)...)
					out = append(out, toEd(c, userChunk("S", chunk))...)
				}
				want = append(want, boundary.line...)
				out = append(out, toEd(c, boundary.line)...)
				if string(out) != string(want) {
					t.Fatalf("got\n%s\nwant\n%s", out, want)
				}
			})
		}
	}
}

// An adapter's settings can arrive between chunks, and say nothing about the message.
func TestReplayHoldsAcrossMetadata(t *testing.T) {
	c := newTestControl(t)
	reopen(t, c, 7, "session/load", "S")
	block := builtBlock(t, "q")
	if out := toEd(c, userChunk("S", block[:30])); len(out) != 0 {
		t.Fatalf("held chunk went out: %s", out)
	}
	metadata := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{"sessionUpdate":"available_commands_update","availableCommands":[]}}}` + "\n")
	if out := toEd(c, metadata); !strings.Contains(string(out), "available_commands_update") {
		t.Fatalf("metadata = %s", out)
	}
	if got := wireTexts(t, toEd(c, userChunk("S", block[30:]+"hi"))); len(got) != 1 || got[0] != "user_message_chunk: hi" {
		t.Fatalf("after metadata: %q", got)
	}
}

// Only a reopen's replay is read this way, only for its own thread, and only until its answer: a
// message a person sends later is theirs whatever it says, and an image in a message ends the wait.
func TestReplayStopsAtItsBoundaries(t *testing.T) {
	block := builtBlock(t, "q")
	live := userChunk("S", block+"hi")

	c := newTestControl(t)
	if out := toEd(c, live); string(out) != string(live) {
		t.Fatalf("outside a replay: %s", out)
	}
	answer := reopen(t, c, 7, "session/load", "S")
	other := userChunk("T", block+"hi")
	if out := toEd(c, other); string(out) != string(other) {
		t.Fatalf("another thread: %s", out)
	}
	toEd(c, answer)
	if out := toEd(c, live); string(out) != string(live) {
		t.Fatalf("after the answer: %s", out)
	}

	c = newTestControl(t)
	reopen(t, c, 7, "session/load", "S")
	toEd(c, userChunk("S", "["))
	image := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{"sessionUpdate":"user_message_chunk","content":{"type":"image","data":"x","mimeType":"image/png"}}}}` + "\n")
	want := string(userChunk("S", "[")) + string(image)
	if out := toEd(c, image); string(out) != want {
		t.Fatalf("an image: %s", out)
	}

	// A thread that restarts mid-replay drops what its retired child sent.
	c = newTestControl(t)
	answer = reopen(t, c, 7, "session/load", "S")
	toEd(c, userChunk("S", "["))
	c.childReset()
	if got := wireTexts(t, toEd(c, answer)); len(got) != 1 || got[0] != "answer" {
		t.Fatalf("an answer after a reset: %q", got)
	}
}

// A chunk that can only be held without limit is shown instead: a box chooses what it sends.
func TestReplayHoldIsBounded(t *testing.T) {
	c := newTestControl(t)
	reopen(t, c, 7, "session/load", "S")
	c.carryBytes = 0
	start := carriedBlockStart + "codex" + carriedBlockIntro
	first := userChunk("S", start)
	if out := toEd(c, first); len(out) != 0 {
		t.Fatalf("the first chunk fits: %s", out)
	}
	big := userChunk("S", strings.Repeat("x", 64<<10))
	if out := toEd(c, big); string(out) != string(first)+string(big) {
		t.Fatalf("past the limit: %d bytes", len(out))
	}
}

// A quoted end marker in the history cannot end the block early and show the rest as the person's.
func TestCarriedBlockEndsOnlyAtItsOwnMarker(t *testing.T) {
	block := builtBlock(t, "they said "+carriedBlockEnd+" and left")
	if length, _ := carriedBlock(block + "hi"); length != len(block) {
		t.Fatalf("block length = %d, want %d", length, len(block))
	}
	if !strings.Contains(block, "and left") || strings.Count(block, carriedBlockEnd) != 1 {
		t.Fatalf("carried text lost or the marker doubled: %q", block)
	}
}

// Every open hears the start message, after the replay, under the editor's id even when the answer
// names a native one, with what is true now.
func TestReopenedThreadHearsTheStartMessageFirst(t *testing.T) {
	c := newTestControl(t)
	c.SetServePorts([]ServePort{{Box: 5173, Host: 41000}})

	for name, method := range map[string]string{"load": "session/load", "resume": "session/resume"} {
		t.Run(name, func(t *testing.T) {
			opening, answer := reopenHearing(t, c, 9, method, "S")
			if got := wireTexts(t, opening); len(got) != 1 || !strings.Contains(got[0], "[localhost:41000](http://localhost:41000)") {
				t.Fatalf("before the request = %q", got)
			}
			if !strings.Contains(string(opening), `"sessionId":"S"`) {
				t.Fatalf("not addressed to the editor's id: %s", opening)
			}
			if out := toEd(c, sessionChunk("S", "agent_message_chunk", "replayed")); strings.Contains(string(out), "localhost") {
				t.Fatalf("start message inside the replay: %s", out)
			}
			if got := wireTexts(t, toEd(c, answer)); len(got) != 1 || got[0] != "answer" {
				t.Fatalf("after the answer = %q", got)
			}
		})
	}

	// A native id in the answer must not start a second message, nor a first in the wrong session.
	_, answer := reopenHearing(t, c, 10, "session/load", "S")
	out := toEd(c, []byte(strings.Replace(string(answer), `{"configOptions":[]}`, `{"sessionId":"native","configOptions":[]}`, 1)))
	if strings.Contains(string(out), "localhost") {
		t.Fatalf("a second message after the answer: %s", out)
	}

	// Its links are the current ones.
	c.SetServePorts([]ServePort{{Box: 5173, Host: 42000}})
	opening, _ := reopenHearing(t, c, 11, "session/load", "S")
	if got := wireTexts(t, opening); len(got) != 1 || !strings.Contains(got[0], "localhost:42000") || strings.Contains(got[0], "41000") {
		t.Fatalf("frames = %q", got)
	}

	// A thread with nothing to say hears nothing.
	quiet := newTestControl(t)
	if opening, _ := reopenHearing(t, quiet, 1, "session/load", "S"); opening != nil {
		t.Fatalf("nothing to say: %s", opening)
	}
}

// What the person has to do comes first in a reopened thread too, while the load itself may be parked.
func TestReopenedThreadHearsTheBoxWaitFirst(t *testing.T) {
	c := newTestControl(t)
	c.SetBoxWait("**Waiting for the box.**")
	c.SetServePorts([]ServePort{{Box: 5173, Host: 41000}})
	opening, _ := reopenHearing(t, c, 2, "session/load", "S")
	got := wireTexts(t, opening)
	if len(got) != 1 || !strings.Contains(got[0], "Waiting for the box") || strings.Index(got[0], "Waiting") > strings.Index(got[0], "localhost") {
		t.Fatalf("opening = %q", got)
	}
}

// A load parked for a box comes through the hook again when it is forwarded. That is one open, so one
// message, and the filter works on the second pass.
func TestParkedReopenSpeaksOnce(t *testing.T) {
	c := newTestControl(t)
	c.SetServePorts([]ServePort{{Box: 5173, Host: 41000}})
	opening, answer := reopenHearing(t, c, 3, "session/load", "S")
	if len(opening) == 0 {
		t.Fatal("the first pass says nothing")
	}
	c.childReset()
	if again, _ := reopenHearing(t, c, 3, "session/load", "S"); again != nil {
		t.Fatalf("the second pass spoke again: %s", again)
	}
	if out := toEd(c, userChunk("S", carriedBlockStart)); len(out) != 0 {
		t.Fatalf("the filter is not armed on the second pass: %s", out)
	}
	if got := wireTexts(t, toEd(c, answer)); len(got) != 2 || got[1] != "answer" {
		t.Fatalf("the answer = %q", got)
	}
	if again, _ := reopenHearing(t, c, 4, "session/load", "S"); len(again) == 0 {
		t.Fatal("the next open says nothing")
	}
}

// A request whose path Coop corrects still speaks first.
func TestReopenWithARewrittenPathHearsItsMessage(t *testing.T) {
	c := newTestControl(t)
	c.repo = t.TempDir()
	c.SetServePorts([]ServePort{{Box: 5173, Host: 41000}})
	request := fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"session/load","params":{"sessionId":"S","cwd":%q,"mcpServers":[]}}`+"\n", c.repo+"/.")
	handled, resp, toAdapter, _ := c.fromEditor([]byte(request))
	if handled || !strings.Contains(string(resp), "localhost:41000") {
		t.Fatalf("handled=%v resp=%s", handled, resp)
	}
	if !strings.Contains(string(toAdapter), fmt.Sprintf(`"cwd":%q`, c.repo)) {
		t.Fatalf("toAdapter=%s", toAdapter)
	}
}

// A failed load's thread is gone in the editor, so what its message took goes to the next thread.
func TestFailedReopenGivesItsMessageBack(t *testing.T) {
	c := newTestControl(t)
	c.AddStartProblems(Problem{Title: "A preset does not load."})
	c.Notify("Port 1 now reaches this box.")
	opening, _ := reopenHearing(t, c, 6, "session/load", "S")
	if !strings.Contains(string(opening), "preset does not load") || !strings.Contains(string(opening), "now reaches") {
		t.Fatalf("opening = %s", opening)
	}
	failed := []byte(`{"jsonrpc":"2.0","id":6,"error":{"code":-32002,"message":"Resource not found"}}` + "\n")
	if got := wireTexts(t, toEd(c, failed)); len(got) != 1 || got[0] != "answer" {
		t.Fatalf("a failed load: %q", got)
	}
	retry, answer := reopenHearing(t, c, 7, "session/load", "S")
	if !strings.Contains(string(retry), "preset does not load") || !strings.Contains(string(retry), "now reaches") {
		t.Fatalf("the retry = %s", retry)
	}
	toEd(c, answer)
	if again, _ := reopenHearing(t, c, 8, "session/load", "S"); strings.Contains(string(again), "preset does not load") || strings.Contains(string(again), "now reaches") {
		t.Fatalf("an open after a good one repeats them: %s", again)
	}
}

// A notice that turns up during a replay waits for it to end, or it would split a replayed message.
func TestNoticeDoesNotLandInsideAReplay(t *testing.T) {
	c := newTestControl(t)
	answer := reopen(t, c, 9, "session/load", "S")
	c.Notify("Port 1 now reaches this box.")
	if got := wireTexts(t, toEd(c, userChunk("S", "hi"))); len(got) != 1 {
		t.Fatalf("during the replay = %q", got)
	}
	toEd(c, answer)
	if got := wireTexts(t, toEd(c, sessionChunk("S", "agent_message_chunk", "later"))); len(got) != 2 || !strings.Contains(got[0], "now reaches") {
		t.Fatalf("after the replay = %q", got)
	}
}

// A thread that was new in this process hears it again when it is reopened.
func TestNewThreadReopenedHearsItAgain(t *testing.T) {
	c := newTestControl(t)
	c.SetServePorts([]ServePort{{Box: 5173, Host: 41000}})
	if got := wireTexts(t, toEd(c, sessionResult("S"))); len(got) != 2 {
		t.Fatalf("session/new frames = %q", got)
	}
	if opening, _ := reopenHearing(t, c, 2, "session/load", "S"); len(wireTexts(t, opening)) != 1 {
		t.Fatalf("reopen says = %s", opening)
	}
}
