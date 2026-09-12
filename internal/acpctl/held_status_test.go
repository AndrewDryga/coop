package acpctl

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ladder"
)

func TestACPSelectedCoolingAccountStatus(t *testing.T) {
	c := newTestControl(t)
	now := time.Now()
	c.limited[accountLimitKey("claude", "personal")] = now.Add(time.Hour)
	later := now.Add(3 * time.Hour)
	c.limited[accountLimitKey("claude", "work")] = later
	_, _, _ = c.SpawnTarget()
	fromEditorPrompt(c, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"S","prompt":[{"type":"text","text":"cancelled"}]}}`))
	c.promptCancelled("S")
	history := len(c.history["S"].entries)
	handled, response, _, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/set_config_option","params":{"sessionId":"S","configId":"coop_account","value":"work"}}`))
	if !handled || !restart || !bytes.Contains(response, []byte("Selected account")) || !bytes.Contains(response, []byte("on Claude Code")) || !bytes.Contains(response, []byte(later.Local().Format("Mon 15:04 MST"))) {
		t.Fatalf("manual selection must disclose the selected account's reset: %s", response)
	}
	if bytes.Contains(response, []byte("Your message will send automatically")) {
		t.Fatal("selection promised to send a cancelled or nonexistent prompt")
	}
	status := c.Hooks().PromptHeld("S")
	var notice struct {
		Params struct {
			SessionID string `json:"sessionId"`
			Update    struct{ Content struct{ Text string } }
		}
	}
	if err := json.Unmarshal(status, &notice); err != nil {
		t.Fatal(err)
	}
	if notice.Params.SessionID != "S" || !strings.Contains(notice.Params.Update.Content.Text, `Waiting for account "work" on Claude Code`) || !strings.Contains(notice.Params.Update.Content.Text, later.Local().Format("Mon 15:04 MST")) || !strings.Contains(notice.Params.Update.Content.Text, "Your message will send automatically.") {
		t.Fatalf("held prompt must identify its real account/reset: %s", status)
	}
	if c.lastPrompt["S"] != nil || c.resend["S"] || len(c.history["S"].entries) != history {
		t.Fatal("presentation claimed prompt ownership or changed history")
	}
	c.limited[accountLimitKey("claude", "work")] = now.Add(-time.Second)
	if status := c.Hooks().PromptHeld("S"); len(status) != 0 {
		t.Fatalf("expired cooldown announced a wait: %s", status)
	}
	// Rotation changes intent before the replacement factory resolves c.target.
	c.sel.Account = ""
	c.autoAccount = "personal"
	if provider, account, at := c.selectedCooldown(); provider != "claude" || account != "personal" || !at.Equal(c.limited[accountLimitKey("claude", "personal")]) {
		t.Fatalf("status used the old factory target instead of current intent: %s %v", account, at)
	}
	c.sel = Selection{Preset: "test"}
	c.rot = ladder.NewRotation([]agents.Target{{Provider: "claude", Model: "model", Accounts: []string{"work"}}})
	c.rot.OnLimit(later, 0, now)
	if provider, account, at := c.selectedCooldown(); provider != "claude" || account != "work" || !at.Equal(later) {
		t.Fatalf("preset status ignored the active model rung's cooldown: %s %v", account, at)
	}
}
