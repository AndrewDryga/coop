package acpproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
)

// noticeIDPrefix marks a message Coop wrote itself. A distinct id per message keeps an editor from
// treating two of them as one.
const noticeIDPrefix = "coop-notice-"

var noticeIDs atomic.Uint64

// threadTail is what the editor is written through, so a Coop message that continues a running agent
// message opens with a rule, whoever built it and however late. An editor appends one to the other, so
// without the rule it reads as more of the agent's reply. Only here is the order the editor sees known.
type threadTail struct {
	mu        sync.Mutex
	out       io.Writer
	agentText map[string]bool // editor sessionId -> the thread's last message is agent text
}

func newThreadTail(out io.Writer) *threadTail {
	return &threadTail{out: out, agentText: map[string]bool{}}
}

func (t *threadTail) Write(lines []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := lines
	if bytes.Contains(lines, []byte(`"session/update"`)) {
		out = make([]byte, 0, len(lines))
		for rest := lines; len(rest) > 0; {
			end := bytes.IndexByte(rest, '\n') + 1
			if end == 0 {
				end = len(rest)
			}
			out = append(out, t.rewrite(rest[:end])...)
			rest = rest[end:]
		}
	}
	if _, err := t.out.Write(out); err != nil {
		return 0, err
	}
	return len(lines), nil
}

// reset says the editor is about to start something new in a thread: the person's message, or a
// thread reopened empty, where a rule would have nothing to set apart.
func (t *threadTail) reset(session string) {
	t.mu.Lock()
	delete(t.agentText, session)
	t.mu.Unlock()
}

func (t *threadTail) rewrite(line []byte) []byte {
	var m struct {
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind      string `json:"sessionUpdate"`
				MessageID string `json:"messageId"`
				Content   struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/update" {
		return line
	}
	session := m.Params.SessionID
	switch update := m.Params.Update; update.Kind {
	case "agent_message_chunk":
		if strings.HasPrefix(update.MessageID, noticeIDPrefix) {
			if t.agentText[session] {
				// SessionMessageLine wrote this line, and the first member named "text" is its text.
				line = bytes.Replace(line, []byte(`"text":"\n\n`), []byte(`"text":"\n\n---\n\n`), 1)
			}
			t.agentText[session] = true
		} else if strings.TrimSpace(update.Content.Text) != "" {
			t.agentText[session] = true
		}
	case "user_message_chunk", "agent_thought_chunk", "tool_call":
		delete(t.agentText, session) // an editor shows these as something else, which sets the next text apart
	}
	return line
}
