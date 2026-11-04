package acpctl

import (
	"encoding/json"
	"strings"
)

// A carried block's fixed format. preambleLocked writes it and carriedBlock reads it back: a replay
// can come from a coop acp that never saw the text it injected, so these markers are all it has.
const (
	carriedBlockStart = "[coop] This thread continues a conversation carried over from "
	carriedBlockIntro = " — the session was re-created (provider switch or lost transcript). The context below is best-effort: message text plus one-line tool narration; tool payloads/results are not included, so re-read anything you need to rely on. Continue the conversation naturally.\n\n--- conversation so far ---\n"
	carriedBlockEnd   = "--- end of carried context ---"
)

// carriedBlock reads text against that format. It returns the length of the whole block text starts
// with, or 0 and whether more text could still make it one.
func carriedBlock(text string) (length int, undecided bool) {
	if !strings.HasPrefix(text, carriedBlockStart) {
		return 0, strings.HasPrefix(carriedBlockStart, text)
	}
	rest := text[len(carriedBlockStart):]
	intro := strings.Index(rest, carriedBlockIntro)
	if intro < 0 {
		// The origin is one line of provider names, so the first line break must be the intro's own.
		line, after, broken := strings.Cut(rest, "\n")
		introLine, introAfter, _ := strings.Cut(carriedBlockIntro, "\n")
		return 0, !broken || strings.HasSuffix(line, introLine) && strings.HasPrefix(introAfter, after)
	}
	if strings.Contains(rest[:intro], "\n") {
		return 0, false
	}
	bodyAt := len(carriedBlockStart) + intro + len(carriedBlockIntro)
	end := strings.Index(text[bodyAt:], carriedBlockEnd)
	if end < 0 {
		return 0, true
	}
	return bodyAt + end + len(carriedBlockEnd), false
}

// replayPosition is where in a replayed user message the next chunk falls.
type replayPosition int

const (
	replayAtStart    replayPosition = iota // the chunk may open a carried block, or continue one
	replayAfterBlock                       // the block is gone, and the line breaks joining it to the person's words go too
	replayInMessage                        // the message is the person's own
)

// threadReplay follows the history an adapter streams for one reopened thread, until it answers.
type threadReplay struct {
	requestID string // the editor's session/load or session/resume
	position  replayPosition
	held      []byte // chunks that may still turn out to be a carried block, as the adapter sent them
	text      string // their text

	toldProblems bool     // its opening message took the session's start problems
	toldNotices  []string // and these queued notices
}

// restart forgets what a retired child sent. The request is still open.
func (r *threadReplay) restart() {
	r.position, r.held, r.text = replayAtStart, nil, ""
}

// beginReplay arms the filter for the editor's request to reopen a thread and returns what the thread
// hears before anything else: its start message, then anything queued. An editor's thread exists by the
// time it asks (Zed registers it before it sends the request), so this comes first however long a box
// takes to answer. A request parked for a box comes through here a second time, and says nothing new.
func (c *Control) beginReplay(id json.RawMessage, session string) []byte {
	c.mu.Lock()
	if replay := c.replays[session]; replay != nil && replay.requestID == string(id) {
		c.mu.Unlock()
		return nil
	}
	replay := &threadReplay{requestID: string(id), toldProblems: !c.problemsTold, toldNotices: c.notices}
	c.replays[session] = replay
	c.notices = nil
	c.mu.Unlock()
	opening := c.startNotice(session)
	if len(replay.toldNotices) > 0 {
		opening = append(opening, c.statusMessage(session, strings.Join(replay.toldNotices, "\n\n"))...)
	}
	return opening
}

// reopenFailed gives back what a failed request's opening message took: an editor drops a thread whose
// load fails, so the next thread must hear it. What was said stays true either way, since it describes
// the session and never claims the thread came back. c.mu is held.
func (c *Control) reopenFailed(replay *threadReplay) {
	if replay.toldProblems {
		c.problemsTold = false
	}
	c.notices = append(replay.toldNotices, c.notices...)
}

// filterReplay hides a carried block from a replayed user message. The block went to the agent as part
// of the first prompt of a re-created session, so its transcript gives it back as what the person said.
// Only a block in the exact format, at the start of a message, is removed, and chunks may split it
// anywhere: they wait until the end marker arrives or something shows they are not a block.
//
// It returns the chunks that have stopped waiting, then the line to handle, nil when there is none, and
// whether that line is the answer to a reopen.
func (c *Control) filterReplay(line []byte) (released, rest []byte, reopenAnswer bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.replays) == 0 {
		return nil, line, false
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Error  json.RawMessage `json:"error"`
		Params struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind    string `json:"sessionUpdate"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil {
		return nil, line, false
	}
	if m.Method == "" {
		for session, replay := range c.replays {
			if replay.requestID == string(m.ID) {
				delete(c.replays, session)
				if len(m.Error) != 0 && string(m.Error) != "null" {
					c.reopenFailed(replay)
				}
				return replay.held, line, true
			}
		}
		return nil, line, false
	}
	replay := c.replays[m.Params.SessionID]
	if replay == nil || m.Method != "session/update" {
		return nil, line, false
	}
	switch m.Params.Update.Kind {
	case "user_message_chunk":
	case "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update", "plan":
		// The conversation moved on, so a message that never reached its end marker was the person's.
		return replay.release(replayAtStart), line, false
	default:
		return nil, line, false // settings and metadata say nothing about the message
	}
	text := m.Params.Update.Content.Text
	if m.Params.Update.Content.Type != "text" {
		return replay.release(replayInMessage), line, false
	}
	switch replay.position {
	case replayInMessage:
		return nil, line, false
	case replayAfterBlock:
		return nil, replay.afterBlock(line, text), false
	}
	replay.text += text
	length, undecided := carriedBlock(replay.text)
	switch {
	case length > 0:
		after := replay.text[length:]
		replay.held, replay.text = nil, ""
		return nil, replay.afterBlock(line, after), false
	case undecided && len(replay.text) <= 2*c.carryBytes+64<<10:
		replay.held = append(replay.held, line...)
		return nil, nil, false
	}
	return replay.release(replayInMessage), line, false
}

// release returns the held chunks, which were not a block after all.
func (r *threadReplay) release(position replayPosition) []byte {
	held := r.held
	r.held, r.text, r.position = nil, "", position
	return held
}

// afterBlock returns line carrying only what follows the block, nil when that is nothing yet. Adapters
// join a prompt's blocks with nothing or with line breaks.
func (r *threadReplay) afterBlock(line []byte, text string) []byte {
	r.position = replayAfterBlock
	text = strings.TrimLeft(text, "\r\n")
	if text == "" {
		return nil
	}
	r.position = replayInMessage
	return withChunkText(line, text)
}

// withChunkText replaces a user_message_chunk's text and keeps every other field.
func withChunkText(line []byte, text string) []byte {
	var message, params, update, content map[string]json.RawMessage
	if json.Unmarshal(line, &message) != nil || json.Unmarshal(message["params"], &params) != nil ||
		json.Unmarshal(params["update"], &update) != nil || json.Unmarshal(update["content"], &content) != nil {
		return line
	}
	content["text"], _ = json.Marshal(text)
	update["content"], _ = json.Marshal(content)
	params["update"], _ = json.Marshal(update)
	message["params"], _ = json.Marshal(params)
	encoded, err := json.Marshal(message)
	if err != nil {
		return line
	}
	return append(encoded, '\n')
}
