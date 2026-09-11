package acpctl

import (
	"bytes"
	"testing"
)

func TestACPControlCancelDuringQuotaWait(t *testing.T) {
	c := newTestControl(t)
	c.accounts = []string{"personal"}
	c.autoAccount = "personal"
	fromEditorPrompt(c, []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S","prompt":[{"type":"text","text":"cancel this question"}]}}`+"\n"))
	c.appendTurn("S", "visible partial answer")
	out, restart := c.toEditor([]byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"limited","data":{"errorKind":"rate_limit"}}}` + "\n"))
	if !restart || !bytes.Contains(out, []byte("Waiting for account")) || len(c.resumePrompt("S")) == 0 {
		t.Fatalf("single-account quota wait was not armed: %s", out)
	}
	if sel := c.selection(); sel.Account != "" || sel.Provider != "" {
		t.Fatalf("automatic quota recovery pinned the selection: %+v", sel)
	}
	c.cached["S"] = []byte(`[{"id":"model","currentValue":"chosen"}]`)
	c.recreate["S"] = true
	c.needPreamble["S"] = true
	c.echoPreamble["S"] = "carried history"
	if got := string(c.Hooks().PromptCancelled("S")); got != "3" {
		t.Fatalf("suppressed retry ID = %q, want 3", got)
	}
	if len(c.resumePrompt("S")) != 0 || c.resend["S"] || c.turnActive["S"] || c.lastPrompt["S"] != nil || c.waits["S"] != 0 {
		t.Fatal("cancel left the quota-limited prompt runnable")
	}
	if len(c.cached["S"]) == 0 || !c.recreate["S"] || !c.needPreamble["S"] || c.echoPreamble["S"] == "" {
		t.Fatal("cancel discarded model/session state or late-echo protection")
	}
	history := c.history["S"]
	if history == nil || len(history.entries) != 2 || history.entries[0].text != "cancel this question" || history.entries[1].text != "visible partial answer" {
		t.Fatalf("cancel lost visible conversation history: %+v", history)
	}
	if got := c.Hooks().PromptCancelled("S"); len(got) != 0 || len(c.history["S"].entries) != 2 {
		t.Fatal("repeated cancel duplicated history or completed the same request twice")
	}
}
