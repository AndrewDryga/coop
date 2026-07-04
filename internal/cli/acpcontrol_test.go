package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/config"
)

func newTestControl(t *testing.T) *acpControl {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"personal", "work"} {
		if err := os.MkdirAll(filepath.Join(dir, "claude", "profiles", p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return newACPControl(&config.Config{ConfigDir: dir}, "claude", "opus[1m]", "", []string{"frontier"})
}

func configOptionIDs(t *testing.T, out []byte) ([]string, map[string]json.RawMessage) {
	t.Helper()
	var m, res map[string]json.RawMessage
	json.Unmarshal(out, &m)
	json.Unmarshal(m["result"], &res)
	var opts []map[string]json.RawMessage
	json.Unmarshal(res["configOptions"], &opts)
	var ids []string
	byID := map[string]json.RawMessage{}
	for _, o := range opts {
		var id string
		json.Unmarshal(o["id"], &id)
		ids = append(ids, id)
		b, _ := json.Marshal(o)
		byID[id] = b
	}
	return ids, res
}

// toEd is the toEditor output bytes only (dropping the restart flag) — most tests just check the line.
func toEd(c *acpControl, line []byte) []byte { out, _ := c.toEditor(line); return out }

// TestACPControlRewrite: coop's toolbar rewrite on a session/new result — drop mode+agent+modes,
// keep model/effort/fast (with the model defaulted to coop's), prepend coop_setup, keep sessionId.
func TestACPControlRewrite(t *testing.T) {
	c := newTestControl(t)
	in := `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","modes":{"currentModeId":"default"},"configOptions":[` +
		`{"id":"mode","type":"select","currentValue":"default","options":[]},` +
		`{"id":"model","type":"select","currentValue":"default","options":[{"value":"opus[1m]","name":"Opus"},{"value":"sonnet","name":"Sonnet"}]},` +
		`{"id":"effort","type":"select","currentValue":"default","options":[]},` +
		`{"id":"fast","type":"select","currentValue":"off","options":[]},` +
		`{"id":"agent","type":"select","currentValue":"default","options":[]}]}}` + "\n"
	ids, res := configOptionIDs(t, toEd(c, []byte(in)))
	if _, ok := res["modes"]; ok {
		t.Error("modes not stripped")
	}
	if string(res["sessionId"]) != `"s1"` {
		t.Errorf("sessionId lost in rewrite: %s", res["sessionId"])
	}
	if len(ids) == 0 || ids[0] != "coop_setup" {
		t.Errorf("coop_setup must be first, got %v", ids)
	}
	for _, bad := range []string{"mode", "agent"} {
		if slices.Contains(ids, bad) {
			t.Errorf("%s dropdown not stripped: %v", bad, ids)
		}
	}
	for _, keep := range []string{"model", "effort", "fast"} {
		if !slices.Contains(ids, keep) {
			t.Errorf("native %s dropdown wrongly dropped: %v", keep, ids)
		}
	}
	// The model defaults to coop's (opus[1m] is one of the adapter's offered values).
	if !strings.Contains(string(toEd(c, []byte(in))), `"currentValue":"opus[1m]"`) {
		t.Error("model currentValue not defaulted to coop's model")
	}
	// coop_setup lists the credentials + presets: intercept values are cred:/preset:-prefixed, while
	// the display rows read "Credential: <name>" / "Preset: <name>".
	rewritten := string(toEd(c, []byte(in)))
	for _, val := range []string{`"cred:personal"`, `"cred:work"`, `"preset:frontier"`} {
		if !strings.Contains(rewritten, val) {
			t.Errorf("coop_setup must carry the intercept value %s: %s", val, rewritten)
		}
	}
	for _, label := range []string{`Credential: personal`, `Preset: frontier`} {
		if !strings.Contains(rewritten, label) {
			t.Errorf("coop_setup rows should be friendly-labeled %q: %s", label, rewritten)
		}
	}
}

// TestACPControlPassthrough: a non-config line (the bulk of ACP traffic) is returned byte-identical.
func TestACPControlPassthrough(t *testing.T) {
	c := newTestControl(t)
	line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}` + "\n")
	if got := toEd(c, line); string(got) != string(line) {
		t.Errorf("passthrough changed a non-config line:\n%s", got)
	}
}

// TestACPControlFromEditor: coop's own selector set is intercepted; a REAL change restarts and the ack
// shows the new currentValue; a NO-OP set (the value it's already on — Zed's default_config_options
// echo at startup) is acked but does NOT restart; a native option set passes straight through.
func TestACPControlFromEditor(t *testing.T) {
	c := newTestControl(t)
	c.sel = "cred:personal" // deterministic starting point
	// A prior session/new rewrite would have cached the option array (coop_setup first).
	c.cached["s"] = json.RawMessage(`[{"id":"coop_setup","currentValue":"cred:personal"},{"id":"model"}]`)

	// A native option set (model/effort/fast) passes through to the adapter untouched.
	if h, _, _ := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":5,"method":"session/set_config_option","params":{"sessionId":"s","configId":"model","value":"sonnet"}}`)); h {
		t.Error("a native model set must pass through (handled=false), not be intercepted")
	}

	// A NO-OP coop_setup (same value) is handled but must NOT restart — else it respawns the box at
	// startup before any transcript, and session/load fails "Resource not found" (the reported bug).
	if h, _, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":6,"method":"session/set_config_option","params":{"sessionId":"s","configId":"coop_setup","value":"cred:personal"}}`)); !h || restart {
		t.Errorf("no-op coop_setup = (handled=%v restart=%v), want handled with NO restart", h, restart)
	}

	// A real change restarts, updates the selection, and the ack echoes the NEW currentValue.
	h, resp, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":7,"method":"session/set_config_option","params":{"sessionId":"s","configId":"coop_setup","value":"cred:work"}}`))
	if !h || !restart {
		t.Errorf("coop_setup change = (handled=%v restart=%v), want both true", h, restart)
	}
	if !strings.Contains(string(resp), `"currentValue":"cred:work"`) {
		t.Errorf("ack must show the new currentValue cred:work (not the stale cache), got: %s", resp)
	}
	if cred, preset := c.selection(); cred != "work" || preset != "" {
		t.Errorf("after cred:work, selection = (%q,%q), want (work, \"\")", cred, preset)
	}

	c.fromEditor([]byte(`{"jsonrpc":"2.0","id":8,"method":"session/set_config_option","params":{"sessionId":"s","configId":"coop_setup","value":"preset:frontier"}}`))
	if cred, preset := c.selection(); cred != "" || preset != "frontier" {
		t.Errorf("after preset:frontier, selection = (%q,%q), want (\"\", frontier)", cred, preset)
	}
}

// TestACPControlSessionReady: for claude, coop force-sets mode=bypassPermissions + its model on the
// adapter, via InjectPrefix ids the proxy swallows.
func TestACPControlSessionReady(t *testing.T) {
	c := newTestControl(t)
	var joined string
	for _, m := range c.sessionReady("s1") {
		joined += string(m)
	}
	for _, want := range []string{"bypassPermissions", "opus[1m]", acpproxy.InjectPrefix, "session/set_config_option"} {
		if !strings.Contains(joined, want) {
			t.Errorf("sessionReady force-sets missing %q:\n%s", want, joined)
		}
	}
}

// TestACPControlSessionReadyNonClaude: mode=bypassPermissions is a claude option, so a non-claude lead
// must NOT get it (yolo comes from autoReply instead); coop's model set still goes out.
func TestACPControlSessionReadyNonClaude(t *testing.T) {
	c := newACPControl(&config.Config{ConfigDir: t.TempDir()}, "codex", "gpt-5", "", nil)
	var joined string
	for _, m := range c.sessionReady("s1") {
		joined += string(m)
	}
	if strings.Contains(joined, "bypassPermissions") {
		t.Errorf("codex must not get claude's mode=bypassPermissions:\n%s", joined)
	}
	if !strings.Contains(joined, "gpt-5") {
		t.Errorf("coop's model set should still go out for codex:\n%s", joined)
	}
}

// TestACPControlAutoReply: coop approves every session/request_permission by selecting the adapter's
// allow option (preferring a lasting allow), replying to the adapter and NOT forwarding to the editor;
// any other agent→editor request passes through untouched.
func TestACPControlAutoReply(t *testing.T) {
	c := newTestControl(t)
	perm := `{"jsonrpc":"2.0","id":9,"method":"session/request_permission","params":{"sessionId":"s1","options":[` +
		`{"optionId":"rej","kind":"reject_once"},{"optionId":"once","kind":"allow_once"},{"optionId":"always","kind":"allow_always"}]}}`
	reply, forward := c.autoReply([]byte(perm))
	if forward {
		t.Error("a permission request must NOT be forwarded to the editor")
	}
	if !strings.Contains(string(reply), `"optionId":"always"`) {
		t.Errorf("autoReply should prefer allow_always, got: %s", reply)
	}
	if !strings.Contains(string(reply), `"outcome":"selected"`) || !strings.Contains(string(reply), `"id":9`) {
		t.Errorf("autoReply must select the option on the same request id, got: %s", reply)
	}
	// A non-permission agent→editor request is left alone.
	if r, fwd := c.autoReply([]byte(`{"jsonrpc":"2.0","id":10,"method":"fs/read_text_file","params":{}}`)); r != nil || !fwd {
		t.Errorf("non-permission request must pass through, got reply=%s forward=%v", r, fwd)
	}
}

// TestACPControlAutoRotate: a rate-limit error on a credential session rotates to the next signed-in
// account and asks for a restart; once every account is cooling it forwards the error unchanged; a
// preset session and non-error lines never credential-rotate.
func TestACPControlAutoRotate(t *testing.T) {
	c := newTestControl(t)
	c.accounts = []string{"personal", "work"} // the temp cfg isn't "authed", so set the rotation set
	c.sel = "cred:personal"

	limitErr := []byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"HTTP 429: usage limit reached"}}` + "\n")
	out, restart := c.toEditor(limitErr)
	if !restart {
		t.Fatal("a rate-limit error on a credential session must trigger a restart (rotation)")
	}
	if cred, _ := c.selection(); cred != "work" {
		t.Errorf("expected rotation to work, selection = %q", cred)
	}
	if !strings.Contains(string(out), "switched to work") {
		t.Errorf("editor should get coop's switched-to note, got: %s", out)
	}

	// Now on work with personal cooling → nowhere free → forward the error unchanged, no restart.
	out2, restart2 := c.toEditor(limitErr)
	if restart2 {
		t.Error("with every account cooling, must not rotate again")
	}
	if !strings.Contains(string(out2), "429") {
		t.Errorf("the original error should pass through when it can't rotate, got: %s", out2)
	}

	// A non-error line is never a rotation.
	if _, r := c.toEditor([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{}}` + "\n")); r {
		t.Error("a non-error line must not trigger a rotation")
	}

	// A preset session doesn't credential-rotate (it rotates via its own models ladder).
	c.sel, c.limited = "preset:frontier", map[string]time.Time{}
	if _, r := c.toEditor(limitErr); r {
		t.Error("a preset session must not credential-rotate")
	}
}

// TestChooseAllow: kind preference and fallbacks.
func TestChooseAllow(t *testing.T) {
	cases := []struct {
		opts []permOption
		want string
	}{
		{[]permOption{{"a", "allow_once"}, {"b", "allow_always"}}, "b"},           // lasting beats one-shot
		{[]permOption{{"a", "reject_once"}, {"y", "allow_once"}}, "y"},            // pick the allow
		{[]permOption{{"no", "reject_once"}, {"allow-it", "custom"}}, "allow-it"}, // id-based allow-ish
		{[]permOption{{"first", "weird"}, {"second", "odd"}}, "first"},            // fallback to first
		{nil, ""}, // nothing offered
	}
	for i, tc := range cases {
		if got := chooseAllow(tc.opts); got != tc.want {
			t.Errorf("case %d: chooseAllow=%q want %q", i, got, tc.want)
		}
	}
}

// TestACPControlAutoResendOnRotate: a captured prompt whose turn rate-limits is re-sent transparently
// on the backup credential — the error is swallowed (no editor message) and the session flagged.
func TestACPControlAutoResendOnRotate(t *testing.T) {
	c := newTestControl(t)
	c.accounts = []string{"personal", "work"}
	c.sel = "cred:personal"

	prompt := []byte(`{"jsonrpc":"2.0","id":"p1","method":"session/prompt","params":{"sessionId":"S","prompt":[{"type":"text","text":"hi"}]}}` + "\n")
	if handled, _, _ := c.fromEditor(prompt); handled {
		t.Fatal("a session/prompt must pass through (handled=false)")
	}
	if c.lastPrompt["S"] == nil || c.promptSession[`"p1"`] != "S" {
		t.Fatalf("prompt not captured: lastPrompt=%v promptSession=%v", c.lastPrompt["S"], c.promptSession)
	}

	limitErr := []byte(`{"jsonrpc":"2.0","id":"p1","error":{"code":-32000,"message":"You've hit your session limit"}}` + "\n")
	out, restart := c.toEditor(limitErr)
	if !restart {
		t.Fatal("a rate-limit error must restart")
	}
	// The error is swallowed; the only thing the editor sees is a config_option_update moving the
	// coop_setup dropdown to the new credential (no chat message).
	if !strings.Contains(string(out), "config_option_update") || !strings.Contains(string(out), `"cred:work"`) {
		t.Errorf("rotate should push a dropdown update to cred:work, got: %s", out)
	}
	if strings.Contains(string(out), `"error"`) || strings.Contains(string(out), "session limit") {
		t.Errorf("the rate-limit error must not reach the editor, got: %s", out)
	}
	if cred, _ := c.selection(); cred != "work" {
		t.Errorf("must rotate to work, got %q", cred)
	}
	if !c.resend["S"] {
		t.Error("session S must be flagged for resend")
	}
	if got := c.resumePrompt("S"); string(got) != string(prompt) {
		t.Errorf("resumePrompt should return the captured prompt, got: %s", got)
	}
	if got := c.resumePrompt("S"); got != nil {
		t.Errorf("resumePrompt must be one-shot, second call got: %s", got)
	}
}

// TestACPControlSuppressesLimitChunk: the rate-limit notice the adapter streams before erroring is
// held and then dropped (not flushed) when the rate-limit error follows — a seamless resend.
func TestACPControlSuppressesLimitChunk(t *testing.T) {
	c := newTestControl(t)
	c.accounts = []string{"personal", "work"}
	c.sel = "cred:personal"
	c.fromEditor([]byte(`{"jsonrpc":"2.0","id":"p1","method":"session/prompt","params":{"sessionId":"S"}}` + "\n"))

	chunk := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"You've hit your session limit"}}}}` + "\n")
	if out, _ := c.toEditor(chunk); out != nil {
		t.Errorf("a rate-limit chunk must be held (not forwarded), got: %s", out)
	}
	if _, restart := c.toEditor([]byte(`{"jsonrpc":"2.0","id":"p1","error":{"message":"You've hit your session limit"}}` + "\n")); !restart {
		t.Fatal("the error must rotate")
	}
	// A later normal chunk for S must not carry the dropped notice.
	upd := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}` + "\n")
	out, _ := c.toEditor(upd)
	if strings.Contains(string(out), "session limit") {
		t.Errorf("the suppressed notice must not resurface, got: %s", out)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("the normal update must pass through, got: %s", out)
	}
}

// TestACPControlFlushesHeldChunkOnContinue: a chunk that merely MENTIONS a limit (no error follows) is
// flushed intact once the turn continues — coop never silently drops legitimate output.
func TestACPControlFlushesHeldChunkOnContinue(t *testing.T) {
	c := newTestControl(t)
	c.accounts = []string{"personal", "work"}
	c.sel = "cred:personal"

	chunk := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"a 429 rate limit means"}}}}` + "\n")
	if out, _ := c.toEditor(chunk); out != nil {
		t.Errorf("a limit-mentioning chunk is held pending the outcome, got: %s", out)
	}
	next := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" too many requests"}}}}` + "\n")
	out, _ := c.toEditor(next)
	if !strings.Contains(string(out), "429 rate limit means") || !strings.Contains(string(out), "too many requests") {
		t.Errorf("the held chunk must be flushed before the continuation, got: %s", out)
	}
	if strings.Count(string(out), "\n") != 2 {
		t.Errorf("expected two newline-delimited frames, got: %s", out)
	}
}

// TestACPControlWaitsForReset: with no free account, coop points at the nearest reset, tells the editor
// it's waiting, flags the resend, and keeps the account marked limited so the factory blocks.
func TestACPControlWaitsForReset(t *testing.T) {
	c := newTestControl(t)
	c.accounts = []string{"personal"} // single account → nowhere to rotate
	c.sel = "cred:personal"
	c.fromEditor([]byte(`{"jsonrpc":"2.0","id":"p1","method":"session/prompt","params":{"sessionId":"S"}}` + "\n"))

	epoch := time.Now().Add(time.Hour).Unix()
	limitErr := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":"p1","error":{"message":"Claude AI usage limit reached|%d"}}`, epoch) + "\n")
	out, restart := c.toEditor(limitErr)
	if !restart {
		t.Fatal("must restart to wait on the reset")
	}
	if !strings.Contains(string(out), "Waiting for a reset on credential personal") {
		t.Errorf("editor should get the waiting status, got: %s", out)
	}
	if !strings.Contains(string(out), "config_option_update") {
		t.Errorf("editor should also get a dropdown update for the account being waited on, got: %s", out)
	}
	if cred, _ := c.selection(); cred != "personal" {
		t.Errorf("selection should stay on personal to wait, got %q", cred)
	}
	if !c.resend["S"] {
		t.Error("session must be flagged for resend after the wait")
	}
	if _, ok := c.limited["personal"]; !ok {
		t.Error("personal must be marked limited so the factory waits")
	}
}

// TestFormatWait: MM:SS under an hour, Hh MMm at/over an hour, clamped at zero.
func TestFormatWait(t *testing.T) {
	cases := map[time.Duration]string{
		90 * time.Second:                      "01:30",
		30 * time.Second:                      "00:30",
		time.Hour + time.Minute + time.Second: "1h01m",
		2*time.Hour + 5*time.Minute:           "2h05m",
		-5 * time.Second:                      "00:00",
	}
	for d, want := range cases {
		if got := formatWait(d); got != want {
			t.Errorf("formatWait(%s) = %q, want %q", d, got, want)
		}
	}
}

// TestWaitForReset: no-op when unlimited, blocks until a near reset, aborts on ctx cancel.
func TestWaitForReset(t *testing.T) {
	c := newTestControl(t)

	start := time.Now()
	c.waitForReset(context.Background(), "personal")
	if time.Since(start) > 100*time.Millisecond {
		t.Error("waitForReset must be a no-op for an unlimited credential")
	}

	c.limited["personal"] = time.Now().Add(60 * time.Millisecond)
	start = time.Now()
	c.waitForReset(context.Background(), "personal")
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Errorf("waitForReset returned too early (%s) — it must wait for the reset", d)
	}

	c.limited["personal"] = time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	c.waitForReset(ctx, "personal")
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("waitForReset must abort on ctx cancel, took %s", d)
	}
}
