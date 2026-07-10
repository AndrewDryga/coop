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
	agents "github.com/AndrewDryga/coop/internal/agent"
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
	return newACPControl(&config.Config{ConfigDir: dir}, "claude", "opus[1m]", "", dir, []string{"frontier"}, nil)
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

// TestACPControlRewriteConfigUpdateNotification: coop's toolbar rewrite must ALSO apply to a
// config_option_update NOTIFICATION (params.update.configOptions), not just a session/new result — it's
// the shape the adapter pushes on a mid-session change and the one coop's replay rebuilds after a
// credential/preset switch. Missing it dropped the coop_setup dropdown from the toolbar after a switch
// (the reported bug: switching profile→credential left only the raw adapter dropdowns).
func TestACPControlRewriteConfigUpdateNotification(t *testing.T) {
	c := newTestControl(t)
	c.sel = "cred:work" // a credential session (preset == ""), so the model retarget applies too
	in := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"config_option_update","configOptions":[` +
		`{"id":"mode","type":"select","currentValue":"default","options":[]},` +
		`{"id":"model","type":"select","currentValue":"default","options":[{"value":"opus[1m]","name":"Opus"},{"value":"sonnet","name":"Sonnet"}]},` +
		`{"id":"effort","type":"select","currentValue":"default","options":[]},` +
		`{"id":"agent","type":"select","currentValue":"default","options":[]}]}}}` + "\n"
	out := toEd(c, []byte(in))
	// The configOptions live at params.update.configOptions in a notification (not m["result"]).
	var m, params, update map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	json.Unmarshal(m["params"], &params)
	if string(params["sessionId"]) != `"s1"` {
		t.Errorf("sessionId lost in the rewrite: %s", params["sessionId"])
	}
	json.Unmarshal(params["update"], &update)
	var opts []map[string]json.RawMessage
	if err := json.Unmarshal(update["configOptions"], &opts); err != nil {
		t.Fatalf("update.configOptions missing/unparseable — the notification wasn't rewritten:\n%s", out)
	}
	var ids []string
	for _, o := range opts {
		var id string
		json.Unmarshal(o["id"], &id)
		ids = append(ids, id)
	}
	if len(ids) == 0 || ids[0] != "coop_setup" {
		t.Errorf("coop_setup must be first in a config_option_update too, got %v", ids)
	}
	for _, bad := range []string{"mode", "agent"} {
		if slices.Contains(ids, bad) {
			t.Errorf("%s dropdown not stripped in a config_option_update: %v", bad, ids)
		}
	}
	s := string(out)
	if !strings.Contains(s, `"currentValue":"cred:work"`) {
		t.Errorf("coop_setup should reflect the active selection cred:work:\n%s", s)
	}
	if !strings.Contains(s, `"currentValue":"opus[1m]"`) {
		t.Errorf("model not retargeted to coop's in a config_option_update:\n%s", s)
	}
}

// TestACPControlInjectsSetupWhenAdapterHasNoConfigOptions: the gemini/codex adapters return a
// session/new result with a sessionId but NO configOptions (only claude-agent-acp emits that toolbar).
// coop must still inject its credential/preset selector so the coop dropdown appears for every agent —
// the reported "gemini shows no dropdown options at all".
func TestACPControlInjectsSetupWhenAdapterHasNoConfigOptions(t *testing.T) {
	c := newTestControl(t)
	// A gemini-shaped session/new result: sessionId + a models field, but no configOptions and no modes.
	in := `{"jsonrpc":"2.0","id":"2","result":{"sessionId":"g1","models":{"currentModelId":"auto","availableModels":[{"modelId":"auto","name":"Auto"}]}}}` + "\n"
	out := toEd(c, []byte(in))
	ids, res := configOptionIDs(t, out)
	if string(res["sessionId"]) != `"g1"` {
		t.Errorf("sessionId lost: %s", res["sessionId"])
	}
	if len(ids) == 0 || ids[0] != "coop_setup" {
		t.Errorf("coop_setup must be injected even when the adapter sends no configOptions, got %v", ids)
	}
	if _, ok := res["models"]; !ok {
		t.Error("the adapter's own models field should be preserved, not dropped")
	}
	if !strings.Contains(string(out), `"preset:frontier"`) {
		t.Errorf("the injected coop_setup should list the presets:\n%s", out)
	}
}

// newGeminiControl is a control whose lead switches its model via the gemini shape (session/new
// `models` field + session/set_model), not a native `model` configOption.
func newGeminiControl(t *testing.T, model string) *acpControl {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"personal", "work"} {
		if err := os.MkdirAll(filepath.Join(dir, "gemini", "profiles", p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return newACPControl(&config.Config{ConfigDir: dir}, "gemini", model, "", dir, []string{"frontier"}, nil)
}

// modelOption is the subset of a synthesized `model` configOption the tests assert on.
type modelOption struct {
	Type         string      `json:"type"`
	CurrentValue string      `json:"currentValue"`
	Options      []acpOption `json:"options"`
}

// findModelOption pulls the `model` configOption out of a rewritten session/new result.
func findModelOption(t *testing.T, res map[string]json.RawMessage) modelOption {
	t.Helper()
	var opts []map[string]json.RawMessage
	if err := json.Unmarshal(res["configOptions"], &opts); err != nil {
		t.Fatalf("configOptions: %v", err)
	}
	for _, o := range opts {
		var id string
		json.Unmarshal(o["id"], &id)
		if id == "model" {
			b, _ := json.Marshal(o)
			var m modelOption
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatal("no model option found")
	return modelOption{}
}

// The exact gemini session/new wire shape verified against @google/gemini-cli's ACP source: a `models`
// field ({availableModels:[{modelId,name,description?}], currentModelId}) and NO native model option.
const geminiSessionNew = `{"jsonrpc":"2.0","id":"2","result":{"sessionId":"g1","models":{"currentModelId":"gemini-2.5-pro","availableModels":[{"modelId":"gemini-2.5-pro","name":"Gemini 2.5 Pro"},{"modelId":"gemini-2.5-flash","name":"Gemini 2.5 Flash","description":"faster"}]}}}` + "\n"

// TestACPControlSynthesizesGeminiModelDropdown: gemini's session/new carries model choices in a
// `models` field with no `model` configOption, so coop synthesizes a coop-owned model select (after
// coop_setup) listing availableModels, defaulting to currentModelId.
func TestACPControlSynthesizesGeminiModelDropdown(t *testing.T) {
	c := newGeminiControl(t, "") // no coop launch-model → currentValue tracks the box's currentModelId
	out := toEd(c, []byte(geminiSessionNew))
	ids, res := configOptionIDs(t, out)
	if len(ids) < 2 || ids[0] != "coop_setup" || !slices.Contains(ids, "model") {
		t.Fatalf("want coop_setup first + a synthesized model option, got %v", ids)
	}
	model := findModelOption(t, res)
	if model.Type != "select" || model.CurrentValue != "gemini-2.5-pro" {
		t.Errorf("model select currentValue = %q (type %q), want gemini-2.5-pro/select", model.CurrentValue, model.Type)
	}
	if len(model.Options) != 2 || model.Options[0].Value != "gemini-2.5-pro" || model.Options[1].Value != "gemini-2.5-flash" {
		t.Errorf("model options = %+v, want the two availableModels by modelId", model.Options)
	}
	if !c.leadUsesSetModel {
		t.Error("leadUsesSetModel must latch once coop synthesizes a model dropdown from a models field")
	}
}

// TestACPControlTranslatesGeminiModelSet: setting coop's synthesized model dropdown is translated into
// the adapter's session/set_model (a live switch, no restart), acked to the editor with the new value,
// and remembered as coop's model so it rides the next box swap.
func TestACPControlTranslatesGeminiModelSet(t *testing.T) {
	c := newGeminiControl(t, "")
	toEd(c, []byte(geminiSessionNew)) // latch leadUsesSetModel + cache the option set

	handled, resp, toAdapter, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/set_config_option","params":{"sessionId":"g1","configId":"model","value":"gemini-2.5-flash"}}`))
	if !handled || restart {
		t.Fatalf("a synthesized model set must be handled without a restart (handled=%v restart=%v)", handled, restart)
	}
	// The adapter gets a session/set_model{sessionId, modelId}, not a set_config_option.
	var inj struct {
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			ModelID   string `json:"modelId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(toAdapter, &inj); err != nil {
		t.Fatalf("no adapter inject: %v (%s)", err, toAdapter)
	}
	if inj.Method != "session/set_model" || inj.Params.SessionID != "g1" || inj.Params.ModelID != "gemini-2.5-flash" {
		t.Errorf("inject = %s, want session/set_model{g1, gemini-2.5-flash}", toAdapter)
	}
	if c.model != "gemini-2.5-flash" {
		t.Errorf("coop should remember the pick for the next swap, c.model = %q", c.model)
	}
	// The editor ack echoes the model option at its new value.
	if !strings.Contains(string(resp), `"gemini-2.5-flash"`) {
		t.Errorf("editor ack should show the new model value:\n%s", resp)
	}
}

// TestACPControlGeminiModelSurvivesSwap: once coop owns a gemini model, sessionReady re-applies it with
// session/set_model on every (re)established session — this is what carries the pick across a box swap.
func TestACPControlGeminiModelSurvivesSwap(t *testing.T) {
	c := newGeminiControl(t, "")
	toEd(c, []byte(geminiSessionNew))
	c.fromEditor([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/set_config_option","params":{"sessionId":"g1","configId":"model","value":"gemini-2.5-flash"}}`))

	msgs := c.sessionReady("g2") // a fresh session on the respawned box
	var found bool
	for _, m := range msgs {
		if strings.Contains(string(m), `"session/set_model"`) && strings.Contains(string(m), `"gemini-2.5-flash"`) {
			found = true
		}
		if strings.Contains(string(m), `"session/set_config_option"`) && strings.Contains(string(m), `"configId":"model"`) {
			t.Errorf("a set_model lead must not force the model via set_config_option: %s", m)
		}
	}
	if !found {
		t.Errorf("sessionReady must re-apply the chosen gemini model via session/set_model, got %v", msgs)
	}
}

// TestACPControlGeminiPresetModelWins: on a preset the ladder owns the model — the synthesized dropdown
// shows the box's current model (never coop's), a live pick is NOT remembered as coop's model, and
// sessionReady forces nothing (so a respawn returns to the preset's rung).
func TestACPControlGeminiPresetModelWins(t *testing.T) {
	c := newGeminiControl(t, "gemini-2.5-pro")
	c.sel = "preset:frontier"
	out := toEd(c, []byte(geminiSessionNew))
	_, res := configOptionIDs(t, out)
	if m := findModelOption(t, res); m.CurrentValue != "gemini-2.5-pro" {
		t.Errorf("on a preset the dropdown shows the box's currentModelId, got %q", m.CurrentValue)
	}
	c.fromEditor([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/set_config_option","params":{"sessionId":"g1","configId":"model","value":"gemini-2.5-flash"}}`))
	if c.model == "gemini-2.5-flash" {
		t.Error("a preset session must not overwrite coop's model with a live pick — the ladder owns it")
	}
	for _, m := range c.sessionReady("g1") {
		if strings.Contains(string(m), `"session/set_model"`) {
			t.Errorf("sessionReady must not force a model on a preset session: %s", m)
		}
	}
}

// TestACPControlCodexNativeModelNotSynthesized: codex-acp emits BOTH a `models` field AND a native
// `model` configOption (verified against codex-acp source), so coop must NOT synthesize its own — the
// native option flows through and a model set stays a native set_config_option (leadUsesSetModel off).
func TestACPControlCodexNativeModelNotSynthesized(t *testing.T) {
	c := newGeminiControl(t, "")
	codexNew := `{"jsonrpc":"2.0","id":"3","result":{"sessionId":"c1","models":{"currentModelId":"gpt-5.5","availableModels":[{"modelId":"gpt-5.5","name":"GPT-5.5"}]},"configOptions":[{"id":"model","type":"select","currentValue":"gpt-5.5","options":[{"value":"gpt-5.5","name":"GPT-5.5"}]}]}}`
	out := toEd(c, []byte(codexNew+"\n"))
	ids, _ := configOptionIDs(t, out)
	// Exactly one model option — the adapter's native one, not a coop duplicate.
	n := 0
	for _, id := range ids {
		if id == "model" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want exactly one (native) model option, got %d in %v", n, ids)
	}
	if c.leadUsesSetModel {
		t.Error("leadUsesSetModel must stay off when the adapter already emits a native model option")
	}
	// A native model set passes through to the adapter (handled=false), never translated.
	if h, _, adapter, _ := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/set_config_option","params":{"sessionId":"c1","configId":"model","value":"gpt-5"}}`)); h || adapter != nil {
		t.Errorf("a native model set must pass through (handled=%v, inject=%s)", h, adapter)
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
	if h, _, _, _ := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":5,"method":"session/set_config_option","params":{"sessionId":"s","configId":"model","value":"sonnet"}}`)); h {
		t.Error("a native model set must pass through (handled=false), not be intercepted")
	}

	// A NO-OP coop_setup (same value) is handled but must NOT restart — else it respawns the box at
	// startup before any transcript, and session/load fails "Resource not found" (the reported bug).
	if h, _, _, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":6,"method":"session/set_config_option","params":{"sessionId":"s","configId":"coop_setup","value":"cred:personal"}}`)); !h || restart {
		t.Errorf("no-op coop_setup = (handled=%v restart=%v), want handled with NO restart", h, restart)
	}

	// A real change restarts, updates the selection, and the ack echoes the NEW currentValue.
	h, resp, _, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":7,"method":"session/set_config_option","params":{"sessionId":"s","configId":"coop_setup","value":"cred:work"}}`))
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

// TestACPControlPresetOwnsModel: on a PRESET selection the preset's lead ladder owns the model —
// sessionReady must not force coop's launch-time model over it, and the toolbar rewrite must show the
// box's currentValue instead of retargeting it.
func TestACPControlPresetOwnsModel(t *testing.T) {
	c := newTestControl(t) // model = opus[1m]
	c.sel = "preset:frontier"

	var joined string
	for _, m := range c.sessionReady("s1") {
		joined += string(m)
	}
	if strings.Contains(joined, `"configId":"model"`) {
		t.Errorf("sessionReady must not force coop's model on a preset session:\n%s", joined)
	}

	in := `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","configOptions":[` +
		`{"id":"model","type":"select","currentValue":"claude-fable-5","options":[{"value":"opus[1m]","name":"Opus"},{"value":"claude-fable-5","name":"Fable"}]}]}}` + "\n"
	out := toEd(c, []byte(in))
	if !strings.Contains(string(out), `"currentValue":"claude-fable-5"`) || strings.Contains(string(out), `"currentValue":"opus[1m]"`) {
		t.Errorf("preset session must show the box's model, not coop's launch model:\n%s", out)
	}

	// Back on a credential, the retarget applies again.
	c.sel = "cred:personal"
	out = toEd(c, []byte(in))
	if !strings.Contains(string(out), `"currentValue":"opus[1m]"`) {
		t.Errorf("credential session should default the model to coop's:\n%s", out)
	}
}

// TestACPControlSessionReadyNonClaude: mode=bypassPermissions is a claude option, so a non-claude lead
// must NOT get it (yolo comes from autoReply instead); coop's model set still goes out.
func TestACPControlSessionReadyNonClaude(t *testing.T) {
	c := newACPControl(&config.Config{ConfigDir: t.TempDir()}, "codex", "gpt-5", "", t.TempDir(), nil, nil)
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
	if handled, _, _, _ := c.fromEditor(prompt); handled {
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

// presetControl is a control on a preset session with a pre-built 2-rung ladder (fable→opus), bypassing
// the disk load (presetRotation reuses c.rot when rotPreset matches the selection). A prompt is
// in-flight so a rotation can transparently re-send it.
func presetControl(t *testing.T) *acpControl {
	t.Helper()
	c := newTestControl(t)
	c.sel = "preset:frontier"
	c.rotPreset = "frontier"
	c.rot = newRotation([]runTarget{
		{model: "claude-fable-5", credential: "personal"},
		{model: "claude-opus-4-8", credential: "personal"},
	})
	// Drive a real session/prompt so promptSession (keyed by the raw id) + lastPrompt are captured the
	// way the wire does — the error below correlates back to it for the transparent re-send.
	prompt := []byte(`{"jsonrpc":"2.0","id":"req1","method":"session/prompt","params":{"sessionId":"sess1","prompt":[{"type":"text","text":"hi"}]}}` + "\n")
	c.fromEditor(prompt)
	return c
}

// TestACPControlPresetLadderFailover: a preset session rotates its MODEL ladder on a rate limit
// (fable→opus) — the step a persistent ACP session can't take via the loop. The rung is advanced, the
// prompt flagged for a transparent re-send, and the raw error is swallowed (coop_setup stays on the
// preset; the model dropdown catches up via the replay). This is the reported "per-model rate limits
// not handled" bug — before the fix maybeRotate returned early for any preset.
func TestACPControlPresetLadderFailover(t *testing.T) {
	c := presetControl(t)
	// The Fable limit error, tagged structurally (errorKind) like the real adapter.
	errLine := []byte(`{"jsonrpc":"2.0","id":"req1","error":{"code":-32603,"message":"You've reached your Fable 5 limit.","data":{"errorKind":"rate_limit"}}}` + "\n")
	out, restart := c.toEditor(errLine)

	if !restart {
		t.Fatal("a preset rate limit should trigger a restart (rotate + resend)")
	}
	if got := c.rot.active(); got.model != "claude-opus-4-8" {
		t.Errorf("rung after the fable limit = %q, want claude-opus-4-8@personal", got)
	}
	if !c.resend["sess1"] {
		t.Error("the prompt must be flagged for a transparent re-send")
	}
	s := string(out)
	if strings.Contains(s, `"error"`) {
		t.Errorf("the raw rate-limit error must not reach the editor:\n%s", s)
	}
	if !strings.Contains(s, "config_option_update") || !strings.Contains(s, `"preset:frontier"`) {
		t.Errorf("expected a config_option_update keeping coop_setup on the preset:\n%s", s)
	}
}

// TestACPControlPresetLadderAllLimited: when every rung is already limited, coop points at the
// soonest-resetting rung and returns a waiting status (the factory blocks before respawning) rather
// than forwarding the error — same shape as the credential all-limited path.
func TestACPControlPresetLadderAllLimited(t *testing.T) {
	c := presetControl(t)
	c.rot.limited[c.rot.targets[1]] = time.Now().Add(30 * time.Minute) // opus already cooling
	errLine := []byte(`{"jsonrpc":"2.0","id":"req1","error":{"message":"reached your Fable 5 limit","data":{"errorKind":"rate_limit"}}}` + "\n")
	out, restart := c.toEditor(errLine)

	if !restart {
		t.Fatal("all rungs limited should still restart (wait + resend)")
	}
	if !c.resend["sess1"] {
		t.Error("the prompt must be flagged for a re-send after the wait")
	}
	s := string(out)
	if strings.Contains(s, `"error"`) {
		t.Errorf("the raw error must not reach the editor when waiting:\n%s", s)
	}
	if !strings.Contains(s, "config_option_update") {
		t.Errorf("expected a config_option_update + wait status:\n%s", s)
	}
}

// TestACPControlPresetSuppressesLimitChunk: the adapter streams the "You've reached your Fable 5
// limit" notice, then a usage_update, THEN the error. The notice must stay buffered across the
// usage_update and be dropped on the rotate — never reaching the editor. The reported regression was
// the usage_update flushing the held notice before the error arrived.
func TestACPControlPresetSuppressesLimitChunk(t *testing.T) {
	c := presetControl(t)
	notice := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"You've reached your Fable 5 limit. Run /usage-credits to continue or switch models with /model."},"messageId":"m1"}}}` + "\n")
	usage := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess1","update":{"sessionUpdate":"usage_update","used":0,"size":200000}}}` + "\n")
	errLine := []byte(`{"jsonrpc":"2.0","id":"req1","error":{"code":-32603,"message":"You've reached your Fable 5 limit.","data":{"errorKind":"rate_limit"}}}` + "\n")

	if out, _ := c.toEditor(notice); out != nil {
		t.Fatalf("the limit notice chunk must be held, not forwarded, got:\n%s", out)
	}
	// A usage_update between the notice and the error must NOT flush the held notice (the bug).
	if out, _ := c.toEditor(usage); strings.Contains(string(out), "reached your Fable 5 limit") {
		t.Errorf("usage_update flushed the held notice — it must stay buffered:\n%s", out)
	}
	// The error rotates and drops the held notice; it never reaches the editor.
	out, restart := c.toEditor(errLine)
	if !restart {
		t.Fatal("the rate-limit error should rotate")
	}
	if strings.Contains(string(out), "reached your Fable 5 limit") {
		t.Errorf("the limit notice leaked on the rotate:\n%s", out)
	}
	if got := c.takeHeld("sess1"); got != nil {
		t.Errorf("the held notice should have been dropped by the rotation, got:\n%s", got)
	}
}

// TestACPServeNotice: the published-port URLs are announced once, on a session/new result (which
// carries sessionId + configOptions) — not on a ConfigOptionUpdate, not twice, and not without URLs.
func TestACPServeNotice(t *testing.T) {
	c := newTestControl(t)
	c.serveURLs = []string{"box :5173 → http://localhost:24187"}

	result := []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"S","configOptions":[{"id":"model"}]}}` + "\n")
	n1 := c.serveNoticeFor(result)
	if n1 == nil || !strings.Contains(string(n1), "localhost:24187") || !strings.Contains(string(n1), "session/update") {
		t.Fatalf("expected a serve notice for the new session, got %s", n1)
	}
	if n2 := c.serveNoticeFor(result); n2 != nil {
		t.Errorf("the serve notice must be one-shot per session, got %s", n2)
	}

	// A ConfigOptionUpdate (configOptions in params, not result) is not a session establishment.
	upd := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S2","update":{"sessionUpdate":"config_option_update","configOptions":[]}}}` + "\n")
	if n := c.serveNoticeFor(upd); n != nil {
		t.Errorf("only a session/new result announces, got %s", n)
	}

	c.serveURLs = nil
	if n := c.serveNoticeFor([]byte(`{"result":{"sessionId":"S3","configOptions":[1]}}` + "\n")); n != nil {
		t.Errorf("no serve URLs → no notice, got %s", n)
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

// presetControlFor is presetControl for an arbitrary lead agent — the structural-limit
// tests build one per provider, because each session classifies by ITS adapter's signals.
func presetControlFor(t *testing.T, lead string) *acpControl {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"personal", "work"} {
		if err := os.MkdirAll(filepath.Join(dir, lead, "profiles", p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := newACPControl(&config.Config{ConfigDir: dir}, lead, "m", "", dir, []string{"frontier"}, nil)
	c.sel = "preset:frontier"
	c.rotPreset = "frontier"
	c.rot = newRotation([]runTarget{
		{model: "m1", credential: "personal"},
		{model: "m2", credential: "personal"},
	})
	prompt := []byte(`{"jsonrpc":"2.0","id":"req1","method":"session/prompt","params":{"sessionId":"sess1","prompt":[{"type":"text","text":"hi"}]}}` + "\n")
	c.fromEditor(prompt)
	return c
}

// TestACPControlStructuralLimits verifies exact structured provider signals, not just prose, drive
// rate-limit rotation — and that each session matches only ITS OWN adapter's signals (the seam:
// acpcontrol carries no provider constants; Agent.ACPRateLimitSignals owns them).
func TestACPControlStructuralLimits(t *testing.T) {
	cases := []struct {
		name    string
		lead    string
		error   string
		restart bool
	}{
		{
			"codex top-level usageLimitExceeded",
			"codex",
			`{"code":-32603,"message":"provider declined the request","codexErrorInfo":"usageLimitExceeded"}`,
			true,
		},
		{
			"codex nested usageLimitExceeded",
			"codex",
			`{"code":-32603,"message":"provider declined the request","data":{"codexErrorInfo":"usageLimitExceeded"}}`,
			true,
		},
		{
			"gemini resource exhausted",
			"gemini",
			`{"code":-32603,"message":"provider declined the request","data":{"code":"RESOURCE_EXHAUSTED"}}`,
			true,
		},
		{
			"claude errorKind rate_limit",
			"claude",
			`{"code":-32603,"message":"provider declined the request","data":{"errorKind":"rate_limit"}}`,
			true,
		},
		{
			"codexErrorInfo field with non-limit value",
			"codex",
			`{"code":-32603,"message":"provider declined the request","codexErrorInfo":"internalServerError"}`,
			false,
		},
		{
			// Ownership: another provider's marker on a claude-led session is foreign — the
			// claude adapter never emits it, so it must not drive a rotation.
			"codex marker is foreign on a claude session",
			"claude",
			`{"code":-32603,"message":"provider declined the request","codexErrorInfo":"usageLimitExceeded"}`,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := presetControlFor(t, tc.lead)
			line := []byte(`{"jsonrpc":"2.0","id":"req1","error":` + tc.error + `}` + "\n")
			_, restart := c.toEditor(line)
			if restart != tc.restart {
				t.Fatalf("restart = %v, want %v", restart, tc.restart)
			}
		})
	}
}

// TestACPErrorLimitHintSignalDriven pins the classifier's contract: it matches whatever
// signals it is HANDED (compactly, key-pinned when a key is given) and carries no
// provider constants of its own — plus the shared output-token axis that needs no
// signals at all, and rate winning over output when both appear.
func TestACPErrorLimitHintSignalDriven(t *testing.T) {
	now := time.Now()
	sig := []agents.ACPSignal{{Value: "quotaBlown"}, {Key: "reason", Value: "too_fast"}}
	if h := acpErrorLimitHint(json.RawMessage(`{"message":"nope","data":{"x":"quotaBlown"}}`), now, sig); !h.limited || h.outputLimited {
		t.Errorf("any-key signal should classify as a rate limit, got %+v", h)
	}
	if h := acpErrorLimitHint(json.RawMessage(`{"message":"nope","data":{"reason":"tooFast"}}`), now, sig); !h.limited {
		t.Errorf("key-pinned signal should compact-match tooFast/too_fast, got %+v", h)
	}
	if h := acpErrorLimitHint(json.RawMessage(`{"message":"nope","data":{"other":"too_fast"}}`), now, sig); h.limited {
		t.Errorf("a key-pinned value under the wrong key must not match, got %+v", h)
	}
	if h := acpErrorLimitHint(json.RawMessage(`{"message":"x","data":{"stopReason":"MAX_TOKENS"}}`), now, nil); !h.limited || !h.outputLimited {
		t.Errorf("the shared output axis needs no signals, got %+v", h)
	}
	both := json.RawMessage(`{"message":"x","data":{"stopReason":"MAX_TOKENS","y":"quotaBlown"}}`)
	if h := acpErrorLimitHint(both, now, sig); !h.limited || h.outputLimited {
		t.Errorf("a structured rate signal outranks the output axis, got %+v", h)
	}
}

func TestACPControlOutputLimitDoesNotRotateOrHold(t *testing.T) {
	c := presetControl(t)
	errLine := []byte(`{"jsonrpc":"2.0","id":"req1","error":{"message":"Output Limit Reached: maximum output length"}}` + "\n")
	out, restart := c.toEditor(errLine)
	if restart {
		t.Fatal("an ACP output limit is not a rate limit and must not rotate")
	}
	if !strings.Contains(string(out), "Output Limit Reached") {
		t.Fatalf("output-limit error should pass through, got: %s", out)
	}

	c = presetControl(t)
	chunk := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Output Limit Reached: maximum output length"}}}}` + "\n")
	out, restart = c.toEditor(chunk)
	if restart {
		t.Fatal("an ACP output-limit chunk is not a rate limit and must not rotate")
	}
	if !strings.Contains(string(out), "Output Limit Reached") {
		t.Fatalf("output-limit chunk should pass through immediately, got: %s", out)
	}
	if got := c.takeHeld("sess1"); got != nil {
		t.Fatalf("output-limit chunk must not be held as a rate-limit notice, got: %s", got)
	}
}

// TestACPFailoverGiveUpCap: the transparent failover must not respawn/wait forever. A free
// rung rotates (and resets the chain); once every rung is limited, each wait counts, and
// past maxACPLimitWaits the REAL limit error is forwarded to the editor instead of another
// silent wait — the ACP analog of the loop's maxLimitWaits.
func TestACPFailoverGiveUpCap(t *testing.T) {
	c := presetControl(t)
	prompt := []byte(`{"jsonrpc":"2.0","id":"req1","method":"session/prompt","params":{"sessionId":"sess1","prompt":[{"type":"text","text":"hi"}]}}` + "\n")
	errLine := []byte(`{"jsonrpc":"2.0","id":"req1","error":{"code":-32603,"message":"reached your limit","data":{"errorKind":"rate_limit"}}}` + "\n")

	// First limit: the second rung is free — a rotation, not a wait.
	if _, restart := c.toEditor(errLine); !restart {
		t.Fatal("first limit should rotate to the free rung")
	}
	// All rungs limited now: exactly maxACPLimitWaits silent waits are allowed…
	for i := 1; i <= maxACPLimitWaits; i++ {
		c.fromEditor(prompt)
		if _, restart := c.toEditor(errLine); !restart {
			t.Fatalf("wait %d/%d should still be a silent wait", i, maxACPLimitWaits)
		}
	}
	// …then the chain is over: the raw error reaches the editor (no restart).
	c.fromEditor(prompt)
	out, restart := c.toEditor(errLine)
	if restart {
		t.Fatalf("wait %d should give up, not restart again", maxACPLimitWaits+1)
	}
	if !strings.Contains(string(out), "reached your limit") {
		t.Fatalf("the real limit error should be forwarded, got: %s", out)
	}
	// The give-up cleared the chain — the next limit starts a fresh cycle (a wait again).
	c.fromEditor(prompt)
	if _, restart := c.toEditor(errLine); !restart {
		t.Fatal("after a give-up, a new limit starts a fresh wait chain")
	}
}

// TestACPHeldChunkFlushedOnErrorResponse: an "approaching your limit" advisory chunk is
// held awaiting the turn's outcome; a NON-limit error response is a terminal outcome too —
// the notice must flush ahead of it (and the tracking clear), not orphan in the buffer.
func TestACPHeldChunkFlushedOnErrorResponse(t *testing.T) {
	c := newTestControl(t)
	prompt := []byte(`{"jsonrpc":"2.0","id":"req1","method":"session/prompt","params":{"sessionId":"sess1","prompt":[{"type":"text","text":"hi"}]}}` + "\n")
	c.fromEditor(prompt)

	warn := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"you are approaching your rate limit"}}}}` + "\n")
	if out, restart := c.toEditor(warn); out != nil || restart {
		t.Fatalf("the limit advisory should be held, got out=%s restart=%v", out, restart)
	}

	errResp := []byte(`{"jsonrpc":"2.0","id":"req1","error":{"code":-1,"message":"tool exploded"}}` + "\n")
	out, restart := c.toEditor(errResp)
	if restart {
		t.Fatal("a non-limit error must not rotate")
	}
	warnIdx := strings.Index(string(out), "approaching your rate limit")
	errIdx := strings.Index(string(out), "tool exploded")
	if warnIdx == -1 || errIdx == -1 || warnIdx > errIdx {
		t.Fatalf("held advisory should flush ahead of the terminal error, got: %s", out)
	}
	if held := c.takeHeld("sess1"); held != nil {
		t.Fatalf("the buffer must be cleared after the flush, still holds: %s", held)
	}
}

// sleepUntilReset shares the wall-clock-remaining logic with the loop's sleepForLimit, so it too is
// suspend-robust and honors ctx cancellation. A canceled ctx must end the wait promptly rather than
// sit on a long monotonic timer.
func TestSleepUntilReset(t *testing.T) {
	// A reset already in the past is a no-op.
	start := time.Now()
	sleepUntilReset(context.Background(), time.Now().Add(-time.Hour), "past")
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Errorf("past reset slept %s, want ~0", el)
	}

	// A canceled ctx bails out of a long wait promptly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	sleepUntilReset(ctx, time.Now().Add(time.Hour), "cred")
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("canceled wait took %s, want prompt return", el)
	}
}

// TestACPControlOpportunisticModelCache: a normal ACP session/new refreshes `coop models`
// for free — the claude configOptions `model` select lands in the per-agent cache as coop
// rewrites the toolbar, so a later `coop models` reads it as live.
func TestACPControlOpportunisticModelCache(t *testing.T) {
	c := newTestControl(t)
	in := `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","configOptions":[` +
		`{"id":"model","type":"select","currentValue":"default","options":[{"value":"opus[1m]","name":"Opus"},{"value":"sonnet","name":"Sonnet"}]}]}}` + "\n"
	toEd(c, []byte(in))
	got, ok := loadModelsCache(c.cfg, "claude")
	if !ok || len(got.Models) != 2 || got.Models[0].ID != "opus[1m]" || got.Models[1].ID != "sonnet" {
		t.Fatalf("claude session/new should cache the model option ids, got (%v, %v)", got, ok)
	}
}

// TestACPControlOpportunisticGeminiCache: the same free refresh for gemini — its `models`
// field (no native model option, coop synthesizes the dropdown) lands in the cache.
func TestACPControlOpportunisticGeminiCache(t *testing.T) {
	c := newGeminiControl(t, "")
	toEd(c, []byte(geminiSessionNew))
	got, ok := loadModelsCache(c.cfg, "gemini")
	if !ok || len(got.Models) != 2 || got.Models[0].ID != "gemini-2.5-pro" || got.Models[1].ID != "gemini-2.5-flash" {
		t.Fatalf("gemini session/new should cache the availableModels ids, got (%v, %v)", got, ok)
	}
}
