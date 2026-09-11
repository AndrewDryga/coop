package acpproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestProxyCancelHeldPrompt(t *testing.T) {
	for _, phase := range []string{"replay", "target_settings"} {
		t.Run(phase, func(t *testing.T) {
			var editor bytes.Buffer
			adapter := &recordingWriteCloser{}
			p := &proxy{
				out:         &editor,
				sessions:    map[string]*sess{"editor": {adapterID: "native", provider: "codex"}},
				restartHeld: map[string]clientLine{},
				pending:     map[string]bool{},
				forceBySess: map[string]*forceChain{},
			}
			if phase == "replay" {
				p.restarting = true
			} else {
				p.child = &Child{In: adapter, Provider: "codex"}
				p.forceBySess["native"] = &forceChain{session: "native", activeID: "setting"}
			}
			p.fromClient([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"editor","prompt":[]}}` + "\n"))
			p.fromClient([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"editor"}}` + "\n"))
			response := parse(editor.Bytes())
			if string(response.ID) != "3" || !bytes.Contains(response.Result, []byte(`"stopReason":"cancelled"`)) {
				t.Fatalf("cancel must complete the held prompt immediately, got %s", editor.Bytes())
			}
			if len(p.restartHeld) != 0 || p.restartHeldLen != 0 {
				t.Fatal("cancelled prompt remains held for replay")
			}
			if phase == "target_settings" {
				chain := p.forceBySess["native"]
				if chain == nil || chain.activeID != "setting" || len(chain.held) != 0 {
					t.Fatal("cancel must drop the prompt without discarding the target-setting gate")
				}
			}
			if adapter.Len() != 0 {
				t.Fatalf("adapter received cancellation for a prompt never admitted: %s", adapter.Bytes())
			}
			p.restarting = false
			p.child = &Child{In: adapter, Provider: "codex"}
			delete(p.forceBySess, "native")
			p.fromClient([]byte(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"editor","prompt":[]}}` + "\n"))
			if !strings.Contains(adapter.String(), `"id":4`) || !strings.Contains(adapter.String(), `"sessionId":"native"`) {
				t.Fatalf("fresh prompt cannot use the cancelled session: %s", adapter.Bytes())
			}
		})
	}
}

func TestProxyCancelReservedAndActivePrompts(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "reserved", true: "active"}[admitted], func(t *testing.T) {
			var editor bytes.Buffer
			adapter := &recordingWriteCloser{}
			child := &Child{In: adapter, Provider: "codex"}
			retryID := json.RawMessage("3")
			p := &proxy{
				out: &editor, child: child, generation: 2, restarting: true,
				sessions: map[string]*sess{"editor": {adapterID: "native", provider: "codex"}},
				pending:  map[string]bool{"3": true},
				sessionReqs: map[string]sessionRequest{"3": {
					method: "session/prompt", editorID: "editor", adapterID: "native", generation: 2, admitted: admitted,
				}},
				hooks: &Hooks{PromptCancelled: func(string) json.RawMessage {
					id := retryID
					retryID = nil
					return id
				}},
			}
			p.fromClient([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"editor"}}` + "\n"))
			if admitted {
				if editor.Len() != 0 || !p.pending["3"] || strings.Count(adapter.String(), "session/cancel") != 1 || !strings.Contains(adapter.String(), `"sessionId":"native"`) {
					t.Fatalf("active cancellation must await the adapter: output=%s wire=%s pending=%v", editor.Bytes(), adapter.Bytes(), p.pending)
				}
				p.pumpChild(child, bufio.NewReader(strings.NewReader(string(cancelledPromptResponse("3")))))
			} else if adapter.Len() != 0 || len(p.pending) != 0 || len(p.sessionReqs) != 0 {
				t.Fatalf("reserved cancellation leaked or retained work: wire=%s pending=%v", adapter.Bytes(), p.pending)
			}
			if got := editor.String(); got != string(cancelledPromptResponse("3")) {
				t.Fatalf("prompt did not receive exactly one cancelled response: %s", got)
			}
			p.fromClient([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"editor"}}` + "\n"))
			if editor.String() != string(cancelledPromptResponse("3")) {
				t.Fatal("repeated cancel duplicated the terminal prompt response")
			}
		})
	}
}
