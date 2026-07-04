package cli

import (
	"encoding/json"
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
