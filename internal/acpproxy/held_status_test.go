package acpproxy

import (
	"bytes"
	"strings"
	"testing"
)

func TestProxyHeldPromptStatusDoesNotAdmitPrompt(t *testing.T) {
	var editor bytes.Buffer
	called := 0
	p := &proxy{
		out: &editor, restarting: true,
		sessions:    map[string]*sess{"S": {adapterID: "native"}},
		restartHeld: map[string]clientLine{},
		hooks: &Hooks{
			PromptHeld: func(sid string) []byte {
				preamble := []byte("queued status\n")
				if sid != "S" {
					t.Fatalf("status session = %q", sid)
				}
				called++
				return preamble
			},
			FromEditor: func([]byte) (bool, []byte, []byte, bool) {
				t.Fatal("held prompt reached controller admission")
				return false, nil, nil, false
			},
			PromptForwarded: func([]byte, bool) { t.Fatal("held prompt was forwarded") },
		},
	}
	p.fromClient([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"S","prompt":[]}}` + "\n"))
	if called != 1 || editor.String() != "queued status\n" || len(p.restartHeld) != 1 {
		t.Fatalf("accepted prompt status=%q calls=%d held=%d", editor.String(), called, len(p.restartHeld))
	}
	p.fromClient([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"S","prompt":[]}}` + "\n"))
	if called != 1 {
		t.Fatal("rejected duplicate announced a queued prompt")
	}
	p.fromClient([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"S"}}` + "\n"))
	p.fromClient([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"S"}}` + "\n"))
	if len(p.restartHeld) != 0 || p.restartHeldLen != 0 || strings.Count(editor.String(), `"stopReason":"cancelled"`) != 1 {
		t.Fatalf("cancel did not complete queued prompt exactly once: %s", editor.String())
	}
}
