package acpctl

import (
	"encoding/json"
	"strings"
	"testing"
)

func noticeText(t *testing.T, line []byte) string {
	t.Helper()
	return strings.Trim(noticeTextRaw(t, line), "\n")
}

// noticeTextRaw is a notice exactly as a thread gets it, with the blank lines that set it apart.
func noticeTextRaw(t *testing.T, line []byte) string {
	t.Helper()
	var notice struct {
		Params struct {
			Update struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &notice); err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	return notice.Params.Update.Content.Text
}

// A new thread and a held message both say what they wait for, ahead of anything else. Once nothing
// waits, neither says it.
func TestBoxWaitIsToldToNewThreadsAndHeldMessages(t *testing.T) {
	c := newTestControl(t)
	if status := c.Hooks().PromptHeld("S"); len(status) != 0 {
		t.Fatalf("a held prompt with nothing to wait for said %s", status)
	}
	c.SetNetwork(NetworkLimit{})
	c.SetBoxWait("Run coop build.")
	if got := noticeText(t, c.Hooks().PromptHeld("S")); got != "Run coop build." {
		t.Fatalf("the held message heard %q", got)
	}
	got := noticeText(t, c.startNoticeFor([]byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"T1"}}`)))
	if !strings.HasPrefix(got, "Run coop build.\n\n---\n\nNothing is published and no project rules apply yet. Once the box is running:\n\n") || !strings.Contains(got, "provider") {
		t.Fatalf("the new thread heard %q", got)
	}
	c.SetBoxWait("")
	if status := c.Hooks().PromptHeld("S"); len(status) != 0 {
		t.Fatalf("a prompt held for another reason said %s", status)
	}
	got = noticeText(t, c.startNoticeFor([]byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"T2"}}`)))
	if strings.Contains(got, "coop build") {
		t.Fatalf("a later thread still heard the wait: %q", got)
	}
}

// With nothing else to say, a thread that opens during a wait still hears it, with no stray space.
func TestBoxWaitIsTheWholeStartNoticeWhenNothingElseApplies(t *testing.T) {
	c := newTestControl(t)
	c.SetBoxWait("Run coop build.")
	if got := noticeText(t, c.startNoticeFor([]byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"T"}}`))); got != "Run coop build." {
		t.Fatalf("the thread heard %q", got)
	}
}
