package acpproxy

// SessionBinding is the durable half of one editor thread: the provider whose transcript store holds
// the conversation, and the native session id that store knows it by. The editor's own stable id is
// the key. Every provider/account switch re-creates the session on the box under a fresh native id,
// so after a few switches the thread is a chain of transcripts of which only the newest is current —
// and a fresh `coop acp` (the editor reopened the thread after the process exited) knows none of it.
// Without a binding its session/load carries the editor id to whatever provider is active, which
// replays the transcript written before the first switch and silently drops everything after.
type SessionBinding struct {
	Provider  string
	AdapterID string
}

// BindingStore persists editor id → SessionBinding across coop acp processes. The proxy calls Save
// after every successful bind (session/new, session/load, a replay rebinding), Forget after
// session/delete, and Lookup once per editor session/load|resume. Implementations must treat a
// missing or unreadable entry as a miss — never as an error the editor sees — and must not trust the
// editor id as a path: it is minted by the adapter inside the box.
type BindingStore interface {
	Lookup(editorID string) (SessionBinding, bool)
	Save(editorID string, binding SessionBinding)
	Forget(editorID string)
}

// bindingChange is one store write queued under p.mu and flushed after it is released, so a slow
// disk never stalls the wire.
type bindingChange struct {
	editorID string
	binding  SessionBinding
	forget   bool
}

func (p *proxy) queueBindingLocked(editorID string, binding SessionBinding, forget bool) {
	if p.bindings == nil || editorID == "" {
		return
	}
	p.bindingQueue = append(p.bindingQueue, bindingChange{editorID: editorID, binding: binding, forget: forget})
}

// flushBindings writes the queued binding changes. Every unlock that can follow a bind or delete
// calls it; the store is the only I/O on this path and the queue is empty in the common case.
func (p *proxy) flushBindings() {
	if p.bindings == nil {
		return
	}
	p.mu.Lock()
	queue := p.bindingQueue
	p.bindingQueue = nil
	p.mu.Unlock()
	for _, change := range queue {
		if change.forget {
			p.bindings.Forget(change.editorID)
		} else {
			p.bindings.Save(change.editorID, change.binding)
		}
	}
}

// lookupBinding resolves a reopened thread, or reports a miss for an unbound or unusable entry.
func (p *proxy) lookupBinding(editorID string) (SessionBinding, bool) {
	if p.bindings == nil || editorID == "" {
		return SessionBinding{}, false
	}
	binding, ok := p.bindings.Lookup(editorID)
	if !ok || binding.Provider == "" || binding.AdapterID == "" {
		return SessionBinding{}, false
	}
	Trace("thread %s is bound to %s native session %s", editorID, binding.Provider, binding.AdapterID)
	return binding, true
}

// holdColdLoad parks a reopened thread's session/load while coop brings up the provider whose
// transcript store holds it — the same switch the Provider dropdown makes. It reports true when the
// line was held (the caller is done): the load re-enters forwardClientControlled from
// releaseRestartHeld once the replacement box is published, and by then the binding's provider is
// the active one. False leaves the normal path to forward the load: the session is already known,
// the active box is already that provider, or coop cannot switch (a preset owns the lead, the
// provider is signed out, the hold queue is full) — then the adapter's own answer is the honest one.
func (p *proxy) holdColdLoad(line []byte, sid string, cold SessionBinding) bool {
	if p.hooks == nil || p.hooks.SelectProvider == nil {
		return false
	}
	p.mu.Lock()
	provider := ""
	if p.child != nil {
		provider = p.child.Provider
	}
	known := p.sessions[sid] != nil
	restarting := p.restarting
	_, duplicate := p.restartHeld[sid]
	p.mu.Unlock()
	if known || duplicate || (provider == cold.Provider && !restarting) {
		return false
	}
	// Controller hooks take their own lock: never call them under p.mu.
	switched := p.hooks.SelectProvider(cold.Provider)
	if !switched && !restarting {
		return false
	}
	p.mu.Lock()
	held := !p.reloading.Load() && len(p.restartHeld) < maxRestartHeldPrompts && p.restartHeldLen+len(line) <= maxRestartHeldBytes
	if held {
		p.restartHeld[sid] = clientLine{line: clone(line), origin: originEditor}
		p.restartHeldLen += len(line)
	}
	p.mu.Unlock()
	if held {
		Trace("holding session/load for reopened thread %s until %s is up", sid, cold.Provider)
	}
	if switched {
		p.triggerRestart()
	}
	return held
}
