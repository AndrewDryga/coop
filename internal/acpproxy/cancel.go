package acpproxy

// cancelPrompt runs under controlMu, just like admission and replay publication.
// A queued prompt belongs to Coop, so Coop must finish it; an active prompt still
// belongs to the adapter, which receives the notification and supplies its result.
func (p *proxy) cancelPrompt(editorID string, line []byte) {
	if editorID == "" {
		return
	}
	p.mu.Lock()
	s := p.sessions[editorID]
	if s == nil || s.closed {
		p.mu.Unlock()
		return
	}
	adapterID := s.adapterID
	held := p.dropRestartHeldLocked(editorID)
	if chain := p.forceBySess[adapterID]; chain != nil {
		held = append(held, chain.held...)
		chain.held = nil // keep model/effort setup intact for the next prompt
	}
	local := map[string]bool{}
	for _, prompt := range held {
		local[string(parse(prompt.line).ID)] = true
	}
	active := map[string]bool{}
	child := p.child
	for id, request := range p.sessionReqs {
		if request.method != "session/prompt" || request.editorID != editorID {
			continue
		}
		if child != nil && request.generation == p.generation && request.admitted {
			active[id] = true
			continue
		}
		delete(p.pending, id)
		delete(p.sessionReqs, id)
		local[id] = true
	}
	p.mu.Unlock()
	if p.hooks != nil && p.hooks.PromptCancelled != nil {
		if id := string(p.hooks.PromptCancelled(editorID)); id != "" && !active[id] {
			local[id] = true
		}
	}
	for _, id := range sortedKeys(local) {
		if id != "" && !active[id] {
			p.writeControllerResponse(cancelledPromptResponse(id))
		}
	}
	if len(active) > 0 {
		if adapterID != editorID {
			line = withSessionID(line, adapterID)
		}
		_, _ = child.In.Write(line)
	}
}

func cancelledPromptResponse(id string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"stopReason":"cancelled"}}` + "\n")
}
