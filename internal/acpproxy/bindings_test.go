package acpproxy

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// memBindings is an in-memory BindingStore that records every write, so a test can assert what the
// proxy would have persisted.
type memBindings struct {
	mu      sync.Mutex
	m       map[string]SessionBinding
	saves   []string
	forgets []string
}

func newMemBindings(seed map[string]SessionBinding) *memBindings {
	b := &memBindings{m: map[string]SessionBinding{}}
	for k, v := range seed {
		b.m[k] = v
	}
	return b
}

func (b *memBindings) Lookup(id string) (SessionBinding, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.m[id]
	return v, ok
}

func (b *memBindings) Save(id string, v SessionBinding) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[id] = v
	b.saves = append(b.saves, id+"→"+v.Provider+":"+v.AdapterID)
}

func (b *memBindings) Forget(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, id)
	b.forgets = append(b.forgets, id)
}

func (b *memBindings) snapshot() (saves, forgets []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.saves...), append([]string(nil), b.forgets...)
}

// A reopened thread (no in-memory session) whose binding names the active provider: the load goes
// out under the bound native id, the adapter's replayed history comes back under the editor's id,
// and the thread continues on that native session.
func TestProxyColdLoadUsesStoredBinding(t *testing.T) {
	store := newMemBindings(map[string]SessionBinding{"S1": {Provider: "claude", AdapterID: "N1"}})
	h := newProxyHarnessWith(t, 1, nil, RunOpts{Bindings: store}, "claude")
	h.initialize(0)

	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"cwd":"/w","sessionId":"S1"}}`)
	load := parse(readLine(t, h.childIn[0]))
	if load.Method != "session/load" || sessionID(load.Params) != "N1" {
		t.Fatalf("box received method=%q session=%q, want session/load on the bound native id N1", load.Method, sessionID(load.Params))
	}
	// History replays ahead of the load result and must already carry the editor's id.
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"N1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Hi"}}}}`)
	if update := string(readLine(t, h.clientOut)); !strings.Contains(update, `"sessionId":"S1"`) || strings.Contains(update, `"N1"`) {
		t.Fatalf("replayed history reached the editor as %s, want the editor id S1", update)
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":2,"result":{}}`)
	if id := idStr(t, readLine(t, h.clientOut)); id != "2" {
		t.Fatalf("load response id = %q, want 2", id)
	}
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`)
	prompt := parse(readLine(t, h.childIn[0]))
	if prompt.Method != "session/prompt" || sessionID(prompt.Params) != "N1" {
		t.Fatalf("prompt reached the box as method=%q session=%q, want session/prompt on N1", prompt.Method, sessionID(prompt.Params))
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	readLine(t, h.clientOut)
	if saves, _ := store.snapshot(); len(saves) == 0 || saves[len(saves)-1] != "S1→claude:N1" {
		t.Fatalf("binding saves = %v, want the load to re-save S1→claude:N1", saves)
	}
	h.shutdown()
}

// No binding: the load is forwarded exactly as before bindings existed.
func TestProxyColdLoadMissForwardsEditorID(t *testing.T) {
	store := newMemBindings(nil)
	h := newProxyHarnessWith(t, 1, nil, RunOpts{Bindings: store}, "claude")
	h.initialize(0)
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"cwd":"/w","sessionId":"S1"}}`)
	if load := parse(readLine(t, h.childIn[0])); sessionID(load.Params) != "S1" {
		t.Fatalf("box received session %q, want the editor id S1 on a store miss", sessionID(load.Params))
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":2,"result":{}}`)
	readLine(t, h.clientOut)
	if saves, _ := store.snapshot(); len(saves) != 1 || saves[0] != "S1→claude:S1" {
		t.Fatalf("binding saves = %v, want the successful load to save S1→claude:S1", saves)
	}
	h.shutdown()
}

// A bound native id the adapter can no longer load: the error reaches the editor and the
// provisional reverse map is dropped, so later box output for that id is not misattributed.
func TestProxyColdLoadFailureDropsProvisionalRemap(t *testing.T) {
	store := newMemBindings(map[string]SessionBinding{"S1": {Provider: "claude", AdapterID: "N1"}})
	h := newProxyHarnessWith(t, 1, nil, RunOpts{Bindings: store}, "claude")
	h.initialize(0)
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"cwd":"/w","sessionId":"S1"}}`)
	readLine(t, h.childIn[0])
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"Resource not found"}}`)
	if resp := string(readLine(t, h.clientOut)); !strings.Contains(resp, `"error"`) || idStr(t, []byte(resp)) != "2" {
		t.Fatalf("editor got %s, want the adapter's error for request 2", resp)
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"N1","update":{"sessionUpdate":"current_mode_update","currentModeId":"default"}}}`)
	if update := string(readLine(t, h.clientOut)); !strings.Contains(update, `"sessionId":"N1"`) {
		t.Fatalf("output after a failed cold load was remapped: %s", update)
	}
	if saves, _ := store.snapshot(); len(saves) != 0 {
		t.Fatalf("a failed load saved a binding: %v", saves)
	}
	h.shutdown()
}

// The binding names another provider than the active box: coop selects it (as the Provider dropdown
// would), holds the load through the restart, and forwards it to the new box under the native id.
func TestProxyColdLoadSwitchesToBoundProvider(t *testing.T) {
	store := newMemBindings(map[string]SessionBinding{"S1": {Provider: "codex", AdapterID: "N1"}})
	var selected atomic.Value
	hooks := &Hooks{SelectProvider: func(provider string) bool {
		prev, _ := selected.Load().(string)
		selected.Store(provider)
		return provider != prev && provider == "codex"
	}}
	h := newProxyHarnessWith(t, 2, hooks, RunOpts{Bindings: store}, "claude", "codex")
	h.initialize(0)

	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"cwd":"/w","sessionId":"S1"}}`)
	// The replacement (codex) box gets the handshake replayed, then the held load — nothing else.
	init := parse(readLine(t, h.childIn[1]))
	if init.Method != "initialize" {
		t.Fatalf("first frame on the codex box = %q, want the replayed initialize", init.Method)
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(init.ID)+`,"result":{}}`)
	load := parse(readLine(t, h.childIn[1]))
	if load.Method != "session/load" || sessionID(load.Params) != "N1" || string(load.ID) != "2" {
		t.Fatalf("codex box received method=%q session=%q id=%s, want the editor's load (id 2) on N1", load.Method, sessionID(load.Params), load.ID)
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":2,"result":{}}`)
	if id := idStr(t, readLine(t, h.clientOut)); id != "2" {
		t.Fatalf("load response id = %q, want 2", id)
	}
	if got, _ := selected.Load().(string); got != "codex" {
		t.Fatalf("SelectProvider was asked for %q, want codex", got)
	}
	if saves, _ := store.snapshot(); len(saves) != 1 || saves[0] != "S1→codex:N1" {
		t.Fatalf("binding saves = %v, want S1→codex:N1 once the codex box bound it", saves)
	}
	h.shutdown()
}

// A fresh session is bound (and saved) as itself; a deleted one is forgotten.
func TestProxyBindingsFollowNewAndDelete(t *testing.T) {
	store := newMemBindings(nil)
	h := newProxyHarnessWith(t, 1, nil, RunOpts{Bindings: store}, "codex")
	h.initialize(0)
	h.newSession(0, "S1")
	if saves, _ := store.snapshot(); len(saves) != 1 || saves[0] != "S1→codex:S1" {
		t.Fatalf("binding saves after session/new = %v, want S1→codex:S1", saves)
	}
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/delete","params":{"sessionId":"S1"}}`)
	readLine(t, h.childIn[0])
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":3,"result":{}}`)
	readLine(t, h.clientOut)
	if _, forgets := store.snapshot(); len(forgets) != 1 || forgets[0] != "S1" {
		t.Fatalf("binding forgets after session/delete = %v, want [S1]", forgets)
	}
	h.shutdown()
}
