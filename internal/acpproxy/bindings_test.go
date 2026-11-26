package acpproxy

import (
	"bytes"
	"encoding/json"
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

// A thread opened and never prompted has no transcript, so reopening it after a reload makes the
// adapter answer "Resource not found". The thread starts afresh on a new native session instead of
// failing: the editor's load succeeds, and its next prompt reaches the new session.
func TestProxyLoadWithNoTranscriptStartsAfresh(t *testing.T) {
	store := newMemBindings(map[string]SessionBinding{"S1": {Provider: "claude", AdapterID: "S1"}})
	h := newProxyHarnessWith(t, 1, nil, RunOpts{Bindings: store}, "claude")
	h.initialize(0)

	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"cwd":"/w","mcpServers":[],"sessionId":"S1"}}`)
	if load := parse(readLine(t, h.childIn[0])); load.Method != "session/load" {
		t.Fatalf("box received %q first", load.Method)
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":2,"error":{"code":-32002,"message":"Resource not found: S1"}}`)
	fresh := parse(readLine(t, h.childIn[0]))
	if fresh.Method != "session/new" || sessionID(fresh.Params) != "" || !strings.Contains(string(fresh.Params), `"cwd":"/w"`) {
		t.Fatalf("after a missing transcript the box got %s %s, want session/new in /w", fresh.Method, fresh.Params)
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":`+string(fresh.ID)+`,"result":{"sessionId":"N2","modes":{"currentModeId":"default"}}}`)
	answer := string(readLine(t, h.clientOut))
	if !strings.Contains(answer, `"id":2`) || !strings.Contains(answer, `"result"`) || strings.Contains(answer, "N2") || strings.Contains(answer, "error") {
		t.Fatalf("editor's load was answered %s, want success without the native id", answer)
	}
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`)
	if prompt := parse(readLine(t, h.childIn[0])); prompt.Method != "session/prompt" || sessionID(prompt.Params) != "N2" {
		t.Fatalf("prompt reached the box as %q on %q, want the fresh session N2", prompt.Method, sessionID(prompt.Params))
	}
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
	readLine(t, h.clientOut)
	if saves, _ := store.snapshot(); len(saves) == 0 || saves[len(saves)-1] != "S1→claude:N2" {
		t.Fatalf("binding saves = %v, want S1→claude:N2", saves)
	}
	h.shutdown()
}

// An unprompted thread can lack a transcript. Zed can reopen before that first turn,
// including after a switch gave the editor thread a different native ID.
func TestProxyReopenUnpromptedThread(t *testing.T) {
	for _, tc := range []struct {
		name, provider, native, detail string
		closed                         bool
	}{
		{"cold Codex", "codex", "N1", "no rollout found for thread id N1", false},
		{"closed Codex", "codex", "S1", "no rollout found for thread id S1", true},
		{"missing Gemini ID", "gemini", "N1", "Invalid session identifier \"N1\".\n  Searched for sessions in /w/chats.", false},
		{"empty Gemini project", "gemini", "N1", "No previous sessions found for this project.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemBindings(map[string]SessionBinding{"S1": {Provider: tc.provider, AdapterID: tc.native}})
			h := newProxyHarnessWith(t, 1, nil, RunOpts{Bindings: store}, tc.provider)
			h.initialize(0)
			if tc.closed {
				h.newSession(0, tc.native)
				writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/close","params":{"sessionId":"S1"}}`)
				readLine(t, h.childIn[0])
				writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":3,"result":{}}`)
				readLine(t, h.clientOut)
			}
			writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":4,"method":"session/load","params":{"cwd":"/w","sessionId":"S1"}}`)
			load := parse(readLine(t, h.childIn[0]))
			if load.Method != "session/load" || sessionID(load.Params) != tc.native {
				t.Fatalf("expected to load %s first, got %s %s", tc.native, load.Method, load.Params)
			}
			detail, _ := json.Marshal(tc.detail)
			writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":4,"error":{"code":-32603,"message":"Internal error","data":{"details":`+string(detail)+`}}}`)
			fresh := parse(readLine(t, h.childIn[0]))
			if fresh.Method != "session/new" || sessionID(fresh.Params) != "" {
				t.Fatalf("expected a fresh session, got %s %s", fresh.Method, fresh.Params)
			}
			writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":`+string(fresh.ID)+`,"result":{"sessionId":"N2"}}`)
			if answer := parse(readLine(t, h.clientOut)); string(answer.ID) != "4" || !responseSucceeded(answer) {
				t.Fatalf("load failed: %+v", answer)
			}
			writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{"sessionId":"S1","prompt":[]}}`)
			if prompt := parse(readLine(t, h.childIn[0])); prompt.Method != "session/prompt" || sessionID(prompt.Params) != "N2" {
				t.Fatalf("prompt missed new session: %+v", prompt)
			}
			writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}`)
			readLine(t, h.clientOut)
			if binding, ok := store.Lookup("S1"); !ok || binding.AdapterID != "N2" || binding.Provider != tc.provider {
				t.Fatalf("wrong replacement binding: %+v", binding)
			}
			h.shutdown()
		})
	}
}

// The same JSON-RPC code covers many adapter failures. A foreign session or an
// internal error about anything else must reach the editor without a rebind.
func TestProxyLoadRefusesUnrelatedFailures(t *testing.T) {
	for _, tc := range []struct {
		name, provider, detail string
		bound                  bool
	}{
		{"unknown thread", "codex", "no rollout found for thread id S1", false},
		{"wrong native ID", "codex", "no rollout found for thread id S1", true},
		{"storage failure", "codex", "permission denied", true},
		{"unknown Gemini project", "gemini", "No previous sessions found for this project.", false},
		{"wrong Gemini ID", "gemini", `Invalid session identifier "S1".`, true},
		{"other provider", "claude", "no rollout found for thread id N1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemBindings(nil)
			if tc.bound {
				store.Save("S1", SessionBinding{Provider: tc.provider, AdapterID: "N1"})
			}
			h := newProxyHarnessWith(t, 1, nil, RunOpts{Bindings: store}, tc.provider)
			h.initialize(0)
			writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"S1","cwd":"/w"}}`)
			readLine(t, h.childIn[0])
			detail, _ := json.Marshal(tc.detail)
			failure := `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"Internal error","data":{"details":` + string(detail) + `}}}`
			writeLine(t, h.children[0].outW, failure)
			if answer := strings.TrimSpace(string(readLine(t, h.clientOut))); answer != failure {
				t.Fatalf("wrong load response: %s", answer)
			}
			h.shutdown()
		})
	}
}

func TestProxyLateLoadFailureLeavesSessionClosed(t *testing.T) {
	h := newProxyHarnessWith(t, 1, nil, RunOpts{}, "codex")
	h.initialize(0)
	h.newSession(0, "S1")
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":3,"method":"session/load","params":{"sessionId":"S1","cwd":"/w"}}`)
	readLine(t, h.childIn[0])
	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":4,"method":"session/close","params":{"sessionId":"S1"}}`)
	readLine(t, h.childIn[0])
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":4,"result":{}}`)
	readLine(t, h.clientOut)
	failure := `{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"data":{"details":"no rollout found for thread id S1"}}}`
	writeLine(t, h.children[0].outW, failure)
	if answer := strings.TrimSpace(string(readLine(t, h.clientOut))); answer != failure {
		t.Fatalf("load pending before close got %s", answer)
	}
	h.shutdown()
}

// The child dies after it found no transcript and before it answered the session/new standing in for
// the load. The editor's load stays pending: the replacement child gets the session/new, and its answer
// is the load's answer.
func TestProxyLoadWithNoTranscriptSurvivesTheChildDying(t *testing.T) {
	store := newMemBindings(map[string]SessionBinding{"S1": {Provider: "claude", AdapterID: "S1"}})
	first, second := newFakeChild(), newFakeChild()
	first.provider, second.provider = "claude", "claude"
	first.mcp, second.mcp = "first-tools", "replacement-tools"
	h := newProxyHarnessChildren(t, []*fakeChild{first, second}, nil, RunOpts{Bindings: store})
	h.initialize(0)

	writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"cwd":"/w","mcpServers":[],"sessionId":"S1"}}`)
	readLine(t, h.childIn[0])
	writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":2,"error":{"code":-32002,"message":"Resource not found: S1"}}`)
	if fresh := parse(readLine(t, h.childIn[0])); fresh.Method != "session/new" || !strings.Contains(string(fresh.Params), "first-tools") {
		t.Fatalf("expected a fresh session with this child's shared tools, got %s %s", fresh.Method, fresh.Params)
	}
	h.children[0].outW.Close()

	var fresh header
	for fresh.Method != "session/new" {
		frame := parse(readLine(t, h.childIn[1]))
		switch frame.Method {
		case "initialize":
			writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{"protocolVersion":1}}`)
		case "session/new":
			fresh = frame
		default:
			t.Fatalf("the replacement got %q before the load's session/new", frame.Method)
		}
	}
	if sessionID(fresh.Params) != "" || !strings.Contains(string(fresh.Params), "replacement-tools") || strings.Contains(string(fresh.Params), "first-tools") || !strings.Contains(string(fresh.Params), `"cwd":"/w"`) {
		t.Fatalf("the replacement's session/new = %s, want the load's params without its id", fresh.Params)
	}
	writeLine(t, h.children[1].outW, `{"jsonrpc":"2.0","id":`+string(fresh.ID)+`,"result":{"sessionId":"N3"}}`)
	answer := string(readLine(t, h.clientOut))
	if !strings.Contains(answer, `"id":2`) || !strings.Contains(answer, `"result"`) || strings.Contains(answer, "error") {
		t.Fatalf("editor's load was answered %s, want success from the replacement", answer)
	}
	h.shutdown()
}

// A proxy that hands off or gives up answers a load still waiting on its fresh session, so the editor
// is never left waiting on a child that is gone.
func TestProxyAnswersAWaitingReloadWhenItStopsReplacing(t *testing.T) {
	for name, stop := range map[string]func(*proxy){
		"handoff": (*proxy).beginReload,
		"give up": (*proxy).failAllPending,
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			p := &proxy{out: &out, reloads: map[string]reloadAsNew{
				InjectPrefix + "reload-1-2": {editorID: "S1", request: json.RawMessage(`2`), generation: 1},
			}}
			stop(p)
			if got := out.String(); !strings.Contains(got, `"id":2`) || !strings.Contains(got, "The agent restarted — send your message again.") {
				t.Fatalf("the waiting load was answered %q, want the retry error", got)
			}
			if len(p.reloads) != 0 {
				t.Fatalf("reloads left after stopping: %v", p.reloads)
			}
		})
	}
}

// Any other load failure, and a fresh session that fails too, reach the editor as the adapter's answer.
func TestProxyLoadFailureOtherThanMissingTranscriptIsForwarded(t *testing.T) {
	for name, tc := range map[string]struct{ first, fresh string }{
		"other error":         {first: `{"code":-32603,"message":"internal"}`},
		"fresh session fails": {first: `{"code":-32002,"message":"Resource not found: S1"}`, fresh: `{"code":-32603,"message":"no"}`},
	} {
		t.Run(name, func(t *testing.T) {
			h := newProxyHarnessWith(t, 1, nil, RunOpts{Bindings: newMemBindings(nil)}, "claude")
			h.initialize(0)
			writeLine(t, h.clientIn, `{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"cwd":"/w","sessionId":"S1"}}`)
			readLine(t, h.childIn[0])
			writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":2,"error":`+tc.first+`}`)
			if tc.fresh != "" {
				fresh := parse(readLine(t, h.childIn[0]))
				writeLine(t, h.children[0].outW, `{"jsonrpc":"2.0","id":`+string(fresh.ID)+`,"error":`+tc.fresh+`}`)
			}
			if answer := string(readLine(t, h.clientOut)); !strings.Contains(answer, `"id":2`) || !strings.Contains(answer, tc.first[9:15]) {
				t.Fatalf("editor got %s, want the adapter's first answer %s", answer, tc.first)
			}
			h.shutdown()
		})
	}
}
