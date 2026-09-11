package acpctl

import "encoding/json"

func (c *Control) promptCancelled(session string) json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	var retry struct {
		ID json.RawMessage `json:"id"`
	}
	if c.resend[session] {
		_ = json.Unmarshal(c.lastPrompt[session], &retry)
	}
	// Keep already-visible output in carried history, but never re-arm this turn
	// when a provider is changed after Cancel. Session/model caches stay intact.
	if len(c.turnText[session]) > 0 {
		c.appendHistoryLocked(session, c.turnProvider[session], "assistant", string(c.turnText[session]))
	}
	delete(c.lastPrompt, session)
	delete(c.resend, session)
	delete(c.heldChunk, session)
	delete(c.waits, session)
	delete(c.turnText, session)
	delete(c.turnProvider, session)
	delete(c.turnActive, session)
	delete(c.toolTitle, session)
	for id, sid := range c.promptSession {
		if sid == session {
			delete(c.promptSession, id)
		}
	}
	return retry.ID
}
