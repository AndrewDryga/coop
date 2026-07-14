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
	return newACPControl(&config.Config{ConfigDir: dir}, "claude", "opus[1m]", dir, acpSelection{}, []string{"frontier"}, nil, false)
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
// keep model/effort/fast (with the model defaulted to coop's), prepend coop's dropdowns, keep sessionId.
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
	if len(ids) < 3 || ids[0] != "coop_preset" || ids[1] != "coop_provider" || ids[2] != "coop_account" {
		t.Errorf("coop's dropdowns must lead (preset, provider, account), got %v", ids)
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
	// The Account dropdown lists the lead's credentials, the Preset dropdown the repo's presets —
	// bare values plus the auto/none placeholders.
	rewritten := string(toEd(c, []byte(in)))
	for _, val := range []string{`"value":"personal"`, `"value":"work"`, `"value":"auto"`, `"value":"frontier"`, `"value":"none"`} {
		if !strings.Contains(rewritten, val) {
			t.Errorf("coop dropdowns must carry %s: %s", val, rewritten)
		}
	}
}

// TestACPControlRewriteConfigUpdateNotification: coop's toolbar rewrite must ALSO apply to a
// config_option_update NOTIFICATION (params.update.configOptions), not just a session/new result — it's
// the shape the adapter pushes on a mid-session change and the one coop's replay rebuilds after a
// credential/preset switch. Missing it dropped coop's dropdowns from the toolbar after a switch
// (the reported bug: switching profile→credential left only the raw adapter dropdowns).
func TestACPControlRewriteConfigUpdateNotification(t *testing.T) {
	c := newTestControl(t)
	c.sel = acpSelection{Account: "work"} // a credential session (preset == ""), so the model retarget applies too
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
	if len(ids) < 3 || ids[0] != "coop_preset" {
		t.Errorf("coop's dropdowns must lead in a config_option_update too, got %v", ids)
	}
	for _, bad := range []string{"mode", "agent"} {
		if slices.Contains(ids, bad) {
			t.Errorf("%s dropdown not stripped in a config_option_update: %v", bad, ids)
		}
	}
	s := string(out)
	if !strings.Contains(s, `"currentValue":"work"`) {
		t.Errorf("the Account dropdown should reflect the active selection work:\n%s", s)
	}
	if !strings.Contains(s, `"currentValue":"opus[1m]"`) {
		t.Errorf("model not retargeted to coop's in a config_option_update:\n%s", s)
	}
}

// TestACPControlInjectsSetupWhenAdapterHasNoConfigOptions: the gemini/codex adapters return a
// session/new result with a sessionId but NO configOptions (only claude-agent-acp emits that toolbar).
// coop must still inject its selectors so the coop dropdowns appear for every agent —
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
	if len(ids) < 3 || ids[0] != "coop_preset" {
		t.Errorf("coop's dropdowns must be injected even when the adapter sends no configOptions, got %v", ids)
	}
	if _, ok := res["models"]; !ok {
		t.Error("the adapter's own models field should be preserved, not dropped")
	}
	if !strings.Contains(string(out), `"value":"frontier"`) {
		t.Errorf("the injected Preset dropdown should list the presets:\n%s", out)
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
	return newACPControl(&config.Config{ConfigDir: dir}, "gemini", model, dir, acpSelection{}, []string{"frontier"}, nil, false)
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
// coop dropdowns) listing availableModels, defaulting to currentModelId.
func TestACPControlSynthesizesGeminiModelDropdown(t *testing.T) {
	c := newGeminiControl(t, "") // no coop launch-model → currentValue tracks the box's currentModelId
	out := toEd(c, []byte(geminiSessionNew))
	ids, res := configOptionIDs(t, out)
	if len(ids) < 4 || ids[0] != "coop_preset" || !slices.Contains(ids, "model") {
		t.Fatalf("want coop dropdowns first + a synthesized model option, got %v", ids)
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
func TestACPControlGeminiPresetHidesModel(t *testing.T) {
	c := newGeminiControl(t, "gemini-2.5-pro")
	c.sel = acpSelection{Preset: "frontier"}
	out := toEd(c, []byte(geminiSessionNew))
	ids, _ := configOptionIDs(t, out)
	if slices.Contains(ids, "model") {
		t.Errorf("on a preset no model dropdown is synthesized — the ladder owns it, got %v", ids)
	}
	// Zed may still replay a persisted model pick: swallowed, never a set_model to the adapter,
	// never recorded as coop's model.
	h, _, inject, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/set_config_option","params":{"sessionId":"g1","configId":"model","value":"gemini-2.5-flash"}}` + "\n"))
	if !h || inject != nil || restart {
		t.Errorf("a model set on a preset must be swallowed (handled, no inject, no restart), got h=%v inject=%s restart=%v", h, inject, restart)
	}
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
	signInCred(t, c.cfg, "claude", "work")
	c.sel = acpSelection{Account: "personal"} // deterministic starting point
	// A prior session/new rewrite would have cached the option array (coop's dropdowns first).
	c.cached["s"] = json.RawMessage(`[{"id":"coop_account","currentValue":"personal"},{"id":"model"}]`)

	// A native option set (model/effort/fast) passes through to the adapter untouched.
	if h, _, _, _ := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":5,"method":"session/set_config_option","params":{"sessionId":"s","configId":"model","value":"sonnet"}}`)); h {
		t.Error("a native model set must pass through (handled=false), not be intercepted")
	}

	// A NO-OP account set (the value it's already on) is handled but must NOT restart — else it respawns
	// the box at startup before any transcript, and session/load fails "Resource not found" (the reported bug).
	if h, _, _, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":6,"method":"session/set_config_option","params":{"sessionId":"s","configId":"coop_account","value":"personal"}}`)); !h || restart {
		t.Errorf("no-op coop_account = (handled=%v restart=%v), want handled with NO restart", h, restart)
	}

	// A real change restarts, updates the selection, and the ack echoes the NEW currentValue.
	h, resp, _, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":7,"method":"session/set_config_option","params":{"sessionId":"s","configId":"coop_account","value":"work"}}`))
	if !h || !restart {
		t.Errorf("coop_account change = (handled=%v restart=%v), want both true", h, restart)
	}
	if !strings.Contains(string(resp), `"currentValue":"work"`) {
		t.Errorf("ack's Account dropdown must show the new currentValue work (not the stale cache), got: %s", resp)
	}
	if sel := c.selection(); sel.Account != "work" || sel.Preset != "" {
		t.Errorf("after account work, selection = %+v", sel)
	}

	_, resp, _, restart = c.fromEditor([]byte(`{"jsonrpc":"2.0","id":8,"method":"session/set_config_option","params":{"sessionId":"s","configId":"coop_preset","value":"frontier"}}`))
	if !restart {
		t.Fatal("entering a preset must restart")
	}
	if sel := c.selection(); sel != (acpSelection{Preset: "frontier"}) {
		t.Errorf("after preset frontier, selection = %+v; preset must clear plain state", sel)
	}
	if ids, _ := configOptionIDs(t, resp); !slices.Equal(ids, []string{coopPresetID}) {
		t.Errorf("active preset ack options = %v, want only %s", ids, coopPresetID)
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
	c.sel = acpSelection{Preset: "frontier"}

	var joined string
	for _, m := range c.sessionReady("s1") {
		joined += string(m)
	}
	if strings.Contains(joined, `"configId":"model"`) {
		t.Errorf("sessionReady must not force coop's model on a preset session:\n%s", joined)
	}

	// On a preset the native dropdowns are hidden outright — the ladder and roles own them.
	in := `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","configOptions":[` +
		`{"id":"model","type":"select","currentValue":"claude-fable-5","options":[{"value":"opus[1m]","name":"Opus"},{"value":"claude-fable-5","name":"Fable"}]},` +
		`{"id":"effort","type":"select","currentValue":"high","options":[]},` +
		`{"id":"fast","type":"select","currentValue":"off","options":[]}]}}` + "\n"
	ids, _ := configOptionIDs(t, toEd(c, []byte(in)))
	for _, id := range ids {
		if id == "model" || id == "effort" || id == "fast" {
			t.Errorf("preset session must hide the native %q dropdown, got %v", id, ids)
		}
	}
	if !slices.Contains(ids, "coop_preset") {
		t.Errorf("coop's own dropdowns must survive the preset filter, got %v", ids)
	}

	// Back on a credential, the natives return and the retarget applies again.
	c.sel = acpSelection{Account: "personal"}
	out := toEd(c, []byte(in))
	if !strings.Contains(string(out), `"currentValue":"opus[1m]"`) {
		t.Errorf("credential session should default the model to coop's:\n%s", out)
	}
}

// TestACPControlPresetSwallowsNativeSet: on a preset a native set_config_option (Zed replaying its
// persisted defaults) is answered by coop and never reaches the adapter; on a credential it passes
// through untouched. Selecting the preset itself acks with the natives already gone — the cache
// can't resurrect them ahead of the restart's box truth.
func TestACPControlPresetSwallowsNativeSet(t *testing.T) {
	c := newTestControl(t)
	c.sel = acpSelection{Account: "personal"}

	// Cache a full native option set, as a session/new rewrite would.
	in := `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","configOptions":[` +
		`{"id":"model","type":"select","currentValue":"opus[1m]","options":[{"value":"opus[1m]","name":"Opus"}]},` +
		`{"id":"effort","type":"select","currentValue":"high","options":[]}]}}` + "\n"
	toEd(c, []byte(in))

	// On a credential a native set passes through to the adapter.
	set := []byte(`{"jsonrpc":"2.0","id":9,"method":"session/set_config_option","params":{"sessionId":"s1","configId":"effort","value":"max"}}` + "\n")
	if h, _, _, _ := c.fromEditor(set); h {
		t.Fatal("a native set on a credential session must pass through (handled=false)")
	}

	// Selecting a preset acks with coop-only options.
	sw := []byte(`{"jsonrpc":"2.0","id":10,"method":"session/set_config_option","params":{"sessionId":"s1","configId":"coop_preset","value":"frontier"}}` + "\n")
	h, ack, _, restart := c.fromEditor(sw)
	if !h || !restart {
		t.Fatalf("selecting a preset must be handled + restart, got h=%v restart=%v", h, restart)
	}
	ids, _ := configOptionIDs(t, ack)
	for _, id := range ids {
		if !strings.HasPrefix(id, "coop_") {
			t.Errorf("the preset-select ack must not resurrect native option %q, got %v", id, ids)
		}
	}

	// Under the preset the same native set is swallowed: handled, nothing to the adapter, no restart.
	if h, _, inject, restart := c.fromEditor(set); !h || inject != nil || restart {
		t.Errorf("a native set on a preset must be swallowed, got h=%v inject=%s restart=%v", h, inject, restart)
	}
}

// TestACPControlSessionReadyNonClaude: mode=bypassPermissions is a claude option, so a non-claude lead
// must NOT get it (yolo comes from autoReply instead); coop's model set still goes out.
func TestACPControlSessionReadyNonClaude(t *testing.T) {
	c := newACPControl(&config.Config{ConfigDir: t.TempDir()}, "codex", "gpt-5", t.TempDir(), acpSelection{}, nil, nil, false)
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
	c.sel = acpSelection{Account: "personal"}

	limitErr := []byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"HTTP 429: usage limit reached"}}` + "\n")
	out, restart := c.toEditor(limitErr)
	if !restart {
		t.Fatal("a rate-limit error on a credential session must trigger a restart (rotation)")
	}
	if sel := c.selection(); sel.Account != "work" {
		t.Errorf("expected rotation to work, selection = %+v", sel)
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
	c.sel, c.limited = acpSelection{Preset: "frontier"}, map[string]time.Time{}
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
	c.sel = acpSelection{Account: "personal"}

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
	// Account dropdown to the new credential (no chat message).
	if !strings.Contains(string(out), "config_option_update") || !strings.Contains(string(out), `"currentValue":"work"`) {
		t.Errorf("rotate should push a dropdown update onto account work, got: %s", out)
	}
	if strings.Contains(string(out), `"error"`) || strings.Contains(string(out), "session limit") {
		t.Errorf("the rate-limit error must not reach the editor, got: %s", out)
	}
	if sel := c.selection(); sel.Account != "work" {
		t.Errorf("must rotate to work, got %+v", sel)
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

// TestACPControlResendOnManualSwitch: a credential/preset switch made while a turn is in flight
// arms the same transparent resend a rate-limit rotation does — the turn completes on the new
// target instead of dying with "agent restarted, please retry".
func TestACPControlResendOnManualSwitch(t *testing.T) {
	c := newTestControl(t)
	c.accounts = []string{"personal", "work"}
	c.sel = acpSelection{Account: "personal"}

	prompt := []byte(`{"jsonrpc":"2.0","id":"p1","method":"session/prompt","params":{"sessionId":"S","prompt":[{"type":"text","text":"hi"}]}}` + "\n")
	c.fromEditor(prompt)

	sw := []byte(`{"jsonrpc":"2.0","id":"s1","method":"session/set_config_option","params":{"sessionId":"S","configId":"coop_account","value":"work"}}` + "\n")
	handled, _, _, restart := c.fromEditor(sw)
	if !handled || !restart {
		t.Fatalf("an account switch must be handled + restart, got handled=%v restart=%v", handled, restart)
	}
	if !c.resend["S"] {
		t.Fatal("the in-flight session must be flagged for resend on a manual switch")
	}
	if got := c.resumePrompt("S"); string(got) != string(prompt) {
		t.Errorf("resumePrompt should return the in-flight prompt, got: %s", got)
	}
}

// TestACPControlNoResendForCompletedTurn: a switch AFTER the turn finished must not re-send the
// last prompt — the terminal response already dropped it from the in-flight tracking.
func TestACPControlNoResendForCompletedTurn(t *testing.T) {
	c := newTestControl(t)
	c.accounts = []string{"personal", "work"}
	c.sel = acpSelection{Account: "personal"}

	c.fromEditor([]byte(`{"jsonrpc":"2.0","id":"p1","method":"session/prompt","params":{"sessionId":"S","prompt":[{"type":"text","text":"hi"}]}}` + "\n"))
	c.toEditor([]byte(`{"jsonrpc":"2.0","id":"p1","result":{"stopReason":"end_turn"}}` + "\n"))

	sw := []byte(`{"jsonrpc":"2.0","id":"s1","method":"session/set_config_option","params":{"sessionId":"S","configId":"coop_account","value":"work"}}` + "\n")
	if _, _, _, restart := c.fromEditor(sw); !restart {
		t.Fatal("the switch itself must still restart")
	}
	if c.resend["S"] {
		t.Fatal("a completed turn must not be flagged for resend")
	}
	if got := c.resumePrompt("S"); got != nil {
		t.Errorf("resumePrompt must return nil after a completed turn, got: %s", got)
	}
}

// TestACPControlSuppressesLimitChunk: the rate-limit notice the adapter streams before erroring is
// held and then dropped (not flushed) when the rate-limit error follows — a seamless resend.
func TestACPControlSuppressesLimitChunk(t *testing.T) {
	c := newTestControl(t)
	c.accounts = []string{"personal", "work"}
	c.sel = acpSelection{Account: "personal"}
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
	c.sel = acpSelection{Account: "personal"}

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
	c.sel = acpSelection{Account: "personal"}
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
	if sel := c.selection(); sel.Account != "personal" {
		t.Errorf("selection should stay on personal to wait, got %+v", sel)
	}
	if !c.resend["S"] {
		t.Error("session must be flagged for resend after the wait")
	}
	if _, ok := c.limited["personal"]; !ok {
		t.Error("personal must be marked limited so the factory waits")
	}
}

// presetControl is a control on a preset session with a pre-built 2-rung ladder (fable→opus), bypassing
// the disk load (presetRotation reuses c.rot when rotFor matches the selection). A prompt is
// in-flight so a rotation can transparently re-send it.
func presetControl(t *testing.T) *acpControl {
	t.Helper()
	c := newTestControl(t)
	c.sel = acpSelection{Preset: "frontier"}
	c.rotFor = c.sel
	c.rot = newRotation([]agents.Target{
		{Provider: "claude", Model: "claude-fable-5", Accounts: []string{"personal"}},
		{Provider: "claude", Model: "claude-opus-4-8", Accounts: []string{"personal"}},
	})
	// Drive a real session/prompt so promptSession (keyed by the raw id) + lastPrompt are captured the
	// way the wire does — the error below correlates back to it for the transparent re-send.
	prompt := []byte(`{"jsonrpc":"2.0","id":"req1","method":"session/prompt","params":{"sessionId":"sess1","prompt":[{"type":"text","text":"hi"}]}}` + "\n")
	c.fromEditor(prompt)
	return c
}

// TestACPControlPresetLadderFailover: a preset session rotates its MODEL ladder on a rate limit
// (fable→opus) — the step a persistent ACP session can't take via the loop. The rung is advanced, the
// prompt flagged for a transparent re-send, and the raw error is swallowed (the Preset selector stays
// active; the model catches up via the replay). This is the reported "per-model rate limits
// not handled" bug — before the fix maybeRotate returned early for any preset.
func TestACPControlPresetLadderFailover(t *testing.T) {
	c := presetControl(t)
	// The Fable limit error, tagged structurally (errorKind) like the real adapter.
	errLine := []byte(`{"jsonrpc":"2.0","id":"req1","error":{"code":-32603,"message":"You've reached your Fable 5 limit.","data":{"errorKind":"rate_limit"}}}` + "\n")
	out, restart := c.toEditor(errLine)

	if !restart {
		t.Fatal("a preset rate limit should trigger a restart (rotate + resend)")
	}
	if got := c.rot.active(); got.Model != "claude-opus-4-8" {
		t.Errorf("rung after the fable limit = %q, want claude-opus-4-8@personal", got)
	}
	if !c.resend["sess1"] {
		t.Error("the prompt must be flagged for a transparent re-send")
	}
	s := string(out)
	if strings.Contains(s, `"error"`) {
		t.Errorf("the raw rate-limit error must not reach the editor:\n%s", s)
	}
	if !strings.Contains(s, "config_option_update") || !strings.Contains(s, `"currentValue":"frontier"`) {
		t.Errorf("expected a config_option_update keeping the Preset dropdown on the preset:\n%s", s)
	}
}

// TestACPControlPresetLadderAllLimited: when every rung is already limited, coop points at the
// soonest-resetting rung and returns a waiting status (the factory blocks before respawning) rather
// than forwarding the error — same shape as the credential all-limited path.
func TestACPControlPresetLadderAllLimited(t *testing.T) {
	c := presetControl(t)
	c.rot.limited[c.rot.targets[1].String()] = time.Now().Add(30 * time.Minute) // opus already cooling
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
	c := newACPControl(&config.Config{ConfigDir: dir}, lead, "m", dir, acpSelection{}, []string{"frontier"}, nil, false)
	c.sel = acpSelection{Preset: "frontier"}
	c.rotFor = c.sel
	c.rot = newRotation([]agents.Target{
		{Provider: lead, Model: "m1", Accounts: []string{"personal"}},
		{Provider: lead, Model: "m2", Accounts: []string{"personal"}},
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

// TestACPErrorLimitHintNestedProseReset pins the codex-acp wire shape captured live on
// 2026-07-10: the JSON-RPC message is a generic "Internal error", and the human notice
// carrying the reset clock time rides in data.message. The classifier must mine the
// nested prose so the wait targets the stated reset, not the 5-minute default cooldown.
func TestACPErrorLimitHintNestedProseReset(t *testing.T) {
	now := time.Date(2026, 7, 10, 14, 27, 0, 0, time.Local)
	raw := json.RawMessage(`{"code":-32603,"message":"Internal error","data":{"message":"You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 4:28 PM.","codexErrorInfo":"usageLimitExceeded"}}`)
	h := acpErrorLimitHint(raw, now, []agents.ACPSignal{{Value: "usageLimitExceeded"}})
	if !h.limited || h.outputLimited {
		t.Fatalf("captured codex limit error must classify as a rate limit, got %+v", h)
	}
	want := time.Date(2026, 7, 10, 16, 28, 0, 0, time.Local)
	if !h.resetAt.Equal(want) {
		t.Errorf("resetAt = %v, want %v (mined from data.message prose)", h.resetAt, want)
	}
	// A nested reset never RE-classifies: without the structured signal or limit prose in
	// the top-level message, an ordinary error stays ordinary even when a nested string
	// parses as a full limit notice (echoed user content must not drive a rotation).
	plain := json.RawMessage(`{"code":-32603,"message":"boom","data":{"note":"You've hit your usage limit. Try again at 4:28 PM."}}`)
	if h := acpErrorLimitHint(plain, now, nil); h.limited || !h.resetAt.IsZero() {
		t.Errorf("nested prose alone must not classify the error as limited, got %+v", h)
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
	c.sel = acpSelection{Account: "personal"}
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

// spawnTarget resolves each selection kind to ONE full target and re-derives the per-lead
// state on a provider change — the cross-provider switch's foundation.
func TestACPSpawnTarget(t *testing.T) {
	c := newTestControl(t)
	signInCred(t, c.cfg, "claude", "work")
	signInCred(t, c.cfg, "codex", "work")

	// A credential selection: current lead + coop's model + the picked account.
	c.sel = acpSelection{Account: "work"}
	tt, ps, ok := c.spawnTarget()
	if !ok || ps != "" || tt.String() != "claude:opus[1m]@work" {
		t.Fatalf("cred selection = (%q, %q, %v), want claude:opus[1m]@work", tt, ps, ok)
	}
	if c.lead != "claude" {
		t.Fatalf("cred selection must not retarget the lead, got %q", c.lead)
	}

	// A provider selection: that provider bare (default model + account), and the control
	// retargets — creds/accounts belong to the new lead, the old model pick dies.
	c.sel = acpSelection{Provider: "codex"}
	tt, ps, ok = c.spawnTarget()
	if !ok || ps != "" || tt.String() != "codex" {
		t.Fatalf("agent selection = (%q, %q, %v), want bare codex", tt, ps, ok)
	}
	if c.lead != "codex" || c.model != "" || c.leadUsesSetModel {
		t.Errorf("retarget: lead=%q model=%q setModel=%v, want codex/\"\"/false", c.lead, c.model, c.leadUsesSetModel)
	}
	if len(c.accounts) != 1 || c.accounts[0] != "work" {
		t.Errorf("retarget accounts = %v, want codex's signed-in [work]", c.accounts)
	}
}

// A preset selection spawns the ACTIVE rung as the full target — a cross-provider rung swaps
// the lead. The rotation is pre-built (bypassing disk) exactly like presetControl does.
func TestACPSpawnTargetCrossProviderRung(t *testing.T) {
	c := newTestControl(t)
	signInCred(t, c.cfg, "gemini", "personal")
	c.sel = acpSelection{Preset: "frontier"}
	c.rotFor = c.sel
	c.rot = newRotation([]agents.Target{
		{Provider: "claude", Model: "claude-fable-5", Accounts: []string{"personal"}},
		{Provider: "gemini", Model: "gemini-3.5-pro", Accounts: []string{"personal"}},
	})
	c.rot.idx = 1 // the ladder rotated onto the gemini rung

	tt, ps, ok := c.spawnTarget()
	if !ok || ps != "frontier" || tt.String() != "gemini:gemini-3.5-pro@personal" {
		t.Fatalf("cross-provider rung = (%q, %q, %v), want gemini:gemini-3.5-pro@personal + preset frontier", tt, ps, ok)
	}
	if c.lead != "gemini" || !slices.Equal(c.accounts, []string{"personal"}) {
		t.Errorf("retarget onto the rung's provider: lead=%q accounts=%v", c.lead, c.accounts)
	}
}

func writeACPTestPreset(t *testing.T, repo, name, body string) {
	t.Helper()
	dir := filepath.Join(repo, ".agent", "presets", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func coopCurrentValues(t *testing.T, raw []json.RawMessage) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, item := range raw {
		var option struct {
			ID           string `json:"id"`
			CurrentValue string `json:"currentValue"`
		}
		if err := json.Unmarshal(item, &option); err != nil {
			t.Fatal(err)
		}
		values[option.ID] = option.CurrentValue
	}
	return values
}

func selectorSet(t *testing.T, c *acpControl, configID, value string) (bool, bool, []string) {
	t.Helper()
	line := []byte(`{"jsonrpc":"2.0","id":"selector","method":"session/set_config_option","params":{"sessionId":"s","configId":"` + configID + `","value":"` + value + `"}}` + "\n")
	handled, ack, _, restart := c.fromEditor(line)
	ids, _ := configOptionIDs(t, ack)
	return handled, restart, ids
}

func TestACPPlainSelectorSwitches(t *testing.T) {
	c := newTestControl(t)
	signInCred(t, c.cfg, "codex", "work")
	if handled, restart, ids := selectorSet(t, c, coopProviderID, "codex"); !handled || !restart || !slices.Equal(ids, []string{coopPresetID, coopProviderID, coopAccountID}) {
		t.Fatalf("plain provider switch = handled %v restart %v options %v", handled, restart, ids)
	}
	if got := c.selection(); got != (acpSelection{Provider: "codex"}) {
		t.Fatalf("plain provider selection = %+v, want codex", got)
	}

	c = newTestControl(t)
	signInCred(t, c.cfg, "claude", "work")
	if handled, restart, ids := selectorSet(t, c, coopAccountID, "work"); !handled || !restart || !slices.Equal(ids, []string{coopPresetID, coopProviderID, coopAccountID}) {
		t.Fatalf("plain account switch = handled %v restart %v options %v", handled, restart, ids)
	}
	if got := c.selection(); got != (acpSelection{Account: "work"}) {
		t.Fatalf("plain account selection = %+v, want work", got)
	}
}

func TestACPEnteringPresetClearsPlainSelection(t *testing.T) {
	c := newTestControl(t)
	signInCred(t, c.cfg, "claude", "personal")
	signInCred(t, c.cfg, "codex", "work")
	writeACPTestPreset(t, c.repo, "frontier", "lead: {agent: claude:fable@personal}\n")
	c.sel = acpSelection{Provider: "codex", Account: "work"}

	handled, restart, ids := selectorSet(t, c, coopPresetID, "frontier")
	if !handled || !restart || !slices.Equal(ids, []string{coopPresetID}) {
		t.Fatalf("entering preset = handled %v restart %v options %v", handled, restart, ids)
	}
	if got := c.selection(); got != (acpSelection{Preset: "frontier"}) {
		t.Fatalf("preset selection = %+v, want preset-only", got)
	}
}

func TestACPZedProviderThenPresetUsesPresetLadder(t *testing.T) {
	c := newTestControl(t)
	signInCred(t, c.cfg, "claude", "personal")
	signInCred(t, c.cfg, "codex", "work")
	writeACPTestPreset(t, c.repo, "frontier", "lead:\n  agent: [claude:fable@personal, codex:gpt-5.5@work]\n")

	if handled, restart, _ := selectorSet(t, c, coopProviderID, "codex"); !handled || !restart {
		t.Fatalf("Zed provider replay = handled %v restart %v", handled, restart)
	}
	if got := c.selection(); got != (acpSelection{Provider: "codex"}) {
		t.Fatalf("after provider replay selection = %+v", got)
	}
	if handled, restart, ids := selectorSet(t, c, coopPresetID, "frontier"); !handled || !restart || !slices.Equal(ids, []string{coopPresetID}) {
		t.Fatalf("Zed preset replay = handled %v restart %v options %v", handled, restart, ids)
	}
	if got := c.selection(); got != (acpSelection{Preset: "frontier"}) {
		t.Fatalf("after preset replay selection = %+v, want preset-only", got)
	}
	if rot := c.presetRotation(); rot == nil {
		t.Fatal("frontier must have a preset rotation")
	}
	target, presetName, ok := c.spawnTarget()
	if !ok || presetName != "frontier" || target.String() != "claude:fable@personal" {
		t.Fatalf("frontier first spawn = (%q, %q, %v), want claude:fable@personal", target, presetName, ok)
	}
}

func TestACPActivePresetRejectsProviderAndAccount(t *testing.T) {
	c := newTestControl(t)
	c.sel = acpSelection{Preset: "frontier"}
	for _, tc := range []struct {
		configID string
		value    string
	}{
		{coopProviderID, "codex"},
		{coopAccountID, "work"},
	} {
		handled, restart, ids := selectorSet(t, c, tc.configID, tc.value)
		if !handled || restart || !slices.Equal(ids, []string{coopPresetID}) {
			t.Errorf("active %s set = handled %v restart %v options %v", tc.configID, handled, restart, ids)
		}
		if got := c.selection(); got != (acpSelection{Preset: "frontier"}) {
			t.Errorf("active %s set changed selection to %+v", tc.configID, got)
		}
	}
}

func TestACPLeavingPresetKeepsEffectiveProvider(t *testing.T) {
	c := newTestControl(t)
	signInCred(t, c.cfg, "codex", "codex-only")
	c.sel = acpSelection{Preset: "frontier"}
	c.mu.Lock()
	c.retargetLocked("codex")
	c.mu.Unlock()
	handled, restart, ids := selectorSet(t, c, coopPresetID, "none")
	if !handled || !restart || !slices.Equal(ids, []string{coopPresetID, coopProviderID, coopAccountID}) {
		t.Fatalf("leaving preset = handled %v restart %v options %v", handled, restart, ids)
	}
	if got := c.selection(); got != (acpSelection{Provider: "codex"}) {
		t.Fatalf("leaving selection = %+v, want provider codex and automatic account", got)
	}
	values := coopCurrentValues(t, c.coopOptions())
	if values[coopProviderID] != "codex" || values[coopAccountID] != "auto" {
		t.Fatalf("plain values after leaving preset = %v", values)
	}
	if got := string(c.coopOptions()[2]); !strings.Contains(got, `"value":"codex-only"`) {
		t.Fatalf("plain account options after leaving did not retarget to codex: %s", got)
	}
}

func TestACPSelectionNormalizesMixedState(t *testing.T) {
	dir := t.TempDir()
	mixed := acpSelection{Provider: "codex", Account: "work", Preset: "frontier"}
	c := newACPControl(&config.Config{ConfigDir: dir}, "claude", "opus", dir, mixed, []string{"frontier"}, nil, false)
	if got := c.selection(); got != (acpSelection{Preset: "frontier"}) {
		t.Fatalf("constructor normalized selection = %+v", got)
	}

	c2 := newACPControl(&config.Config{ConfigDir: dir}, "claude", "opus", dir, acpSelection{}, []string{"frontier"}, nil, false)
	c2.restore(ctrlSnapshot{Selection: mixed, Lead: "codex"})
	if got := c2.selection(); got != (acpSelection{Preset: "frontier"}) {
		t.Fatalf("restored normalized selection = %+v", got)
	}

	c3 := newTestControl(t)
	c3.sel = mixed // simulate state persisted by the retired composable contract
	if handled, restart, ids := selectorSet(t, c3, coopProviderID, "codex"); !handled || restart || !slices.Equal(ids, []string{coopPresetID}) {
		t.Fatalf("mixed-state provider replay = handled %v restart %v options %v, want normalized no-op", handled, restart, ids)
	}
	if got := c3.selection(); got != (acpSelection{Preset: "frontier"}) {
		t.Fatalf("mixed state after provider replay = %+v, want preset-only", got)
	}
}

// The controller must begin in the launch's preset state; otherwise the first toolbar says None
// while the inner box has already mounted the preset.
func TestACPInitialPresetSelection(t *testing.T) {
	c := newTestControl(t)
	signInCred(t, c.cfg, "claude", "work")
	writeACPTestPreset(t, c.repo, "frontier", "lead: {agent: claude:fable@work}\n")
	c.sel = acpSelection{Preset: "frontier"}
	target, presetName, ok := c.spawnTarget()
	if !ok || target.String() != "claude:fable@work" || presetName != "frontier" {
		t.Fatalf("initial preset target = (%q, %q, %v)", target, presetName, ok)
	}
	values := coopCurrentValues(t, c.coopOptions())
	if values[coopPresetID] != "frontier" || len(values) != 1 {
		t.Errorf("initial preset-owned values = %v", values)
	}
}

// The Provider dropdown offers the current lead plus other SIGNED-IN providers (never
// unsigned ones, absent under fusion); each plain dropdown changes its own selection field, and a
// value that could never spawn is refused instead of sending the respawn loop chasing it.
func TestACPProviderSelector(t *testing.T) {
	c := newTestControl(t)
	signInCred(t, c.cfg, "claude", "work")
	signInCred(t, c.cfg, "codex", "work")

	optValues := func(raw json.RawMessage) []string {
		var opt struct {
			ID      string      `json:"id"`
			Options []acpOption `json:"options"`
		}
		if err := json.Unmarshal(raw, &opt); err != nil {
			t.Fatal(err)
		}
		var vals []string
		for _, o := range opt.Options {
			vals = append(vals, o.Value)
		}
		return vals
	}
	coop := c.coopOptions()
	if len(coop) != 3 {
		t.Fatalf("want preset+provider+account dropdowns, got %d", len(coop))
	}
	// Preset leads: it's the top-level selector (it embeds provider, model, effort, roles).
	if preset := optValues(coop[0]); !slices.Contains(preset, "none") || !slices.Contains(preset, "frontier") {
		t.Errorf("Preset dropdown %v must offer none + the presets", preset)
	}
	provider := optValues(coop[1])
	if !slices.Contains(provider, "claude") || !slices.Contains(provider, "codex") {
		t.Errorf("Provider dropdown %v must offer the lead and the signed-in codex", provider)
	}
	// Values are grammar tokens; the LABELS are the product names.
	if raw := string(coop[1]); !strings.Contains(raw, `"name":"Claude Code"`) || !strings.Contains(raw, `"name":"Codex"`) {
		t.Errorf("Provider dropdown labels must be product names:\n%s", raw)
	}
	if slices.Contains(provider, "gemini") || slices.Contains(provider, "grok") {
		t.Errorf("Provider dropdown %v must not offer unsigned providers", provider)
	}
	if account := optValues(coop[2]); !slices.Contains(account, "auto") || !slices.Contains(account, "work") {
		t.Errorf("Account dropdown %v must offer auto + the lead's accounts", account)
	}

	// A fusion governor gets no Provider dropdown at all.
	c.fusion = true
	if fus := c.coopOptions(); len(fus) != 2 {
		t.Errorf("fusion must drop the Provider dropdown, got %d options", len(fus))
	}
	set := func(configID, value string) (bool, bool) {
		line := []byte(`{"jsonrpc":"2.0","id":"sx","method":"session/set_config_option","params":{"sessionId":"sess1","configId":"` + configID + `","value":"` + value + `"}}` + "\n")
		handled, _, _, restart := c.fromEditor(line)
		return handled, restart
	}
	if handled, restart := set(coopProviderID, "codex"); !handled || restart {
		t.Errorf("fusion provider set should be a refused ack, got handled=%v restart=%v", handled, restart)
	}
	c.fusion = false

	// A bogus provider is refused — acked, no restart, selection unchanged.
	if handled, restart := set("coop_provider", "bogus"); !handled || restart || c.sel.Provider == "bogus" {
		t.Errorf("bogus provider: handled=%v restart=%v sel=%+v, want a refused ack", handled, restart, c.sel)
	}
	// A real signed-in provider restarts onto it.
	if _, restart := set("coop_provider", "codex"); !restart || c.sel.Provider != "codex" {
		t.Errorf("provider set: restart=%v sel=%+v, want provider codex", restart, c.sel)
	}
	// The Account dropdown switches to one of the (current) lead's credentials.
	if _, restart := set("coop_account", "work"); !restart || c.sel.Account != "work" {
		t.Errorf("account set: restart=%v sel=%+v, want account work", restart, c.sel)
	}
	// Auto clears an explicit account selection; unknown accounts are refused no-ops.
	if _, restart := set("coop_account", "auto"); !restart || c.sel.Account != "" {
		t.Errorf("auto should clear the selection and restart: restart=%v sel=%+v", restart, c.sel)
	}
	if _, restart := set("coop_account", "ghost"); restart || c.sel.Account != "" {
		t.Errorf("unknown account: restart=%v sel=%+v, want a refused ack", restart, c.sel)
	}
	if _, restart := set("coop_account", "work"); !restart {
		t.Fatal("restoring the work pin should restart")
	}
	// Preset on clears plain state; while active Provider and Account replay is a no-op.
	if _, restart := set("coop_preset", "frontier"); !restart || c.sel != (acpSelection{Preset: "frontier"}) {
		t.Errorf("preset set: restart=%v sel=%+v, want preset-only", restart, c.sel)
	}
	if _, restart := set("coop_provider", "claude"); restart || c.sel != (acpSelection{Preset: "frontier"}) {
		t.Errorf("active provider replay: restart=%v sel=%+v, want no-op", restart, c.sel)
	}
	if _, restart := set("coop_account", "work"); restart || c.sel != (acpSelection{Preset: "frontier"}) {
		t.Errorf("active account replay: restart=%v sel=%+v, want no-op", restart, c.sel)
	}
	if _, restart := set("coop_preset", "none"); !restart || c.sel != (acpSelection{Provider: "codex"}) {
		t.Errorf("preset none: restart=%v sel=%+v, want effective codex + auto", restart, c.sel)
	}
}

// The carried history records (user, assistant) pairs: fromEditor captures the prompt text,
// toEditor accumulates the turn's chunks and flushes on the prompt's terminal response —
// bounded per entry and per session.
func TestACPHistoryCapture(t *testing.T) {
	c := newTestControl(t)
	prompt := []byte(`{"jsonrpc":"2.0","id":"r1","method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"what is two plus two?"}]}}` + "\n")
	c.fromEditor(prompt)
	for _, chunk := range []string{"it is ", "four."} {
		line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + chunk + `"}}}}` + "\n")
		c.captureTurn(line)
	}
	c.captureTurn([]byte(`{"jsonrpc":"2.0","id":"r1","result":{"stopReason":"end_turn"}}` + "\n"))

	c.mu.Lock()
	h := c.history["s1"]
	c.mu.Unlock()
	if h == nil || len(h.entries) != 2 {
		t.Fatalf("history = %+v, want the (user, assistant) pair", h)
	}
	if h.entries[0].role != "user" || h.entries[0].text != "what is two plus two?" {
		t.Errorf("user entry = %+v", h.entries[0])
	}
	if h.entries[1].role != "assistant" || h.entries[1].text != "it is four." {
		t.Errorf("assistant entry = %+v", h.entries[1])
	}
	if h.lead != "claude" {
		t.Errorf("history origin = %q, want the lead it ran on", h.lead)
	}
}

// History is byte-bounded: a long assistant turn keeps its TAIL, and a session over the cap
// evicts oldest-first instead of growing without bound.
func TestACPHistoryBounds(t *testing.T) {
	c := newTestControl(t)
	c.mu.Lock()
	long := strings.Repeat("x", historyEntryBytes) + "THE-END"
	c.turnText["s1"] = nil
	c.mu.Unlock()
	// Feed one oversized turn through the chunk path.
	line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + long + `"}}}}` + "\n")
	c.fromEditor([]byte(`{"jsonrpc":"2.0","id":"r1","method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"go"}]}}` + "\n"))
	c.captureTurn(line)
	c.captureTurn([]byte(`{"jsonrpc":"2.0","id":"r1","result":{}}` + "\n"))
	c.mu.Lock()
	got := c.history["s1"].entries[1].text
	c.mu.Unlock()
	if len(got) != historyEntryBytes || !strings.HasSuffix(got, "THE-END") {
		t.Errorf("assistant entry len=%d suffix=%q — want the %d-byte TAIL kept", len(got), got[max(0, len(got)-7):], historyEntryBytes)
	}
	// Session budget (COOP_ACP_CARRY_TOKENS × 4 bytes): the oldest entries evict, the eviction
	// is remembered, and the preamble says so instead of implying completeness.
	c.mu.Lock()
	c.carryBytes = 8 << 10 // shrink the budget so the test doesn't shovel megabytes
	for i := 0; i < 40; i++ {
		c.appendHistoryLocked("s2", "user", strings.Repeat("y", 1024)+itoa(i))
	}
	h := c.history["s2"]
	if h.size > c.carryBytes+historyEntryBytes {
		t.Errorf("history size %d blew the budget %d", h.size, c.carryBytes)
	}
	if strings.HasSuffix(h.entries[0].text, "0") {
		t.Error("oldest entry survived eviction")
	}
	if !h.evicted {
		t.Error("eviction must be remembered for the preamble's omission marker")
	}
	c.appendHistoryLocked("s2", "assistant", "latest answer") // trailing user entry would be dropped from the preamble
	pre := c.preambleLocked("s2")
	c.mu.Unlock()
	if !strings.Contains(pre, "earlier context omitted") {
		t.Errorf("preamble must name the omission after eviction:\n%s", pre[:200])
	}
}

// After SessionRecreated, the editor's NEXT prompt is rewritten with the labeled preamble —
// once. A session that reloaded fine never gets one.
func TestACPPreambleOnNextPrompt(t *testing.T) {
	c := newTestControl(t)
	// Prior conversation, recorded while the session ran on claude.
	c.mu.Lock()
	c.appendHistoryLocked("s1", "user", "explain the bug")
	c.appendHistoryLocked("s1", "assistant", "it is a race in the loop")
	c.mu.Unlock()
	c.sessionRecreated("s1")

	prompt := []byte(`{"jsonrpc":"2.0","id":"r9","method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"now fix it"}]}}` + "\n")
	handled, _, toAdapter, restart := c.fromEditor(prompt)
	if handled || restart {
		t.Fatalf("a prompt rewrite must not be handled/restart (got %v/%v)", handled, restart)
	}
	s := string(toAdapter)
	if s == "" {
		t.Fatal("expected a rewritten prompt carrying the preamble")
	}
	for _, want := range []string{"carried over from claude", "explain the bug", "it is a race in the loop", "now fix it", `"id":"r9"`} {
		if !strings.Contains(s, want) {
			t.Errorf("rewritten prompt missing %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "now fix it") != 1 {
		t.Errorf("the outgoing prompt must not be duplicated into its own preamble:\n%s", s)
	}
	// One-shot: the following prompt goes through untouched.
	prompt2 := []byte(`{"jsonrpc":"2.0","id":"r10","method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"thanks"}]}}` + "\n")
	if _, _, toAdapter2, _ := c.fromEditor(prompt2); toAdapter2 != nil {
		t.Errorf("second prompt after the re-create must pass through, got rewrite: %s", toAdapter2)
	}
	// A session that never re-created is never wrapped.
	other := []byte(`{"jsonrpc":"2.0","id":"r11","method":"session/prompt","params":{"sessionId":"s2","prompt":[{"type":"text","text":"hi"}]}}` + "\n")
	if _, _, toAdapter3, _ := c.fromEditor(other); toAdapter3 != nil {
		t.Errorf("an intact session must not be wrapped, got: %s", toAdapter3)
	}
}

// A transparent resend after a re-create carries the preamble too — the rate-limit rotation
// landed on another provider mid-turn, and the in-flight prompt is the fresh thread's first.
func TestACPPreambleOnResend(t *testing.T) {
	c := newTestControl(t)
	prompt := []byte(`{"jsonrpc":"2.0","id":"r1","method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"summarize the incident"}]}}` + "\n")
	c.fromEditor(prompt) // records lastPrompt + the user history entry
	c.mu.Lock()
	c.appendHistoryLocked("s1", "assistant", "half an answer before the limit")
	c.resend["s1"] = true
	c.mu.Unlock()
	c.sessionRecreated("s1")

	out := string(c.resumePrompt("s1"))
	for _, want := range []string{"carried over from claude", "half an answer before the limit", "summarize the incident", `"id":"r1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("resent prompt missing %q:\n%s", want, out)
		}
	}
	if c.resumePrompt("s1") != nil {
		t.Error("resend is one-shot")
	}
	// Without a re-create, a resend stays byte-identical to the original prompt.
	c2 := newTestControl(t)
	c2.fromEditor(prompt)
	c2.mu.Lock()
	c2.resend["s1"] = true
	c2.mu.Unlock()
	if got := string(c2.resumePrompt("s1")); strings.Contains(got, "carried over") {
		t.Errorf("same-store resend must not grow a preamble: %s", got)
	}
}

// Tool calls ride the carried history as one-line narration — title remembered from the
// initial tool_call, the line emitted on its terminal update, payloads never included.
func TestACPHistoryToolNarration(t *testing.T) {
	c := newTestControl(t)
	c.fromEditor([]byte(`{"jsonrpc":"2.0","id":"r1","method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"fix the bug"}]}}` + "\n"))
	feed := func(l string) { c.captureTurn([]byte(l + "\n")) }
	feed(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Read main.go","kind":"read","status":"pending"}}}`)
	feed(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"looking…"}}}}`)
	feed(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}}`)
	feed(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"t2","title":"Run tests","kind":"execute","status":"pending"}}}`)
	feed(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"t2","status":"failed"}}}`)
	feed(`{"jsonrpc":"2.0","id":"r1","result":{"stopReason":"end_turn"}}`)

	c.mu.Lock()
	h := c.history["s1"]
	c.mu.Unlock()
	if h == nil || len(h.entries) != 2 {
		t.Fatalf("history = %+v, want (user, assistant)", h)
	}
	turn := h.entries[1].text
	for _, want := range []string{"looking…", "[tool] Read main.go — completed", "[tool] Run tests — failed"} {
		if !strings.Contains(turn, want) {
			t.Errorf("turn narrative missing %q:\n%s", want, turn)
		}
	}
	c.mu.Lock()
	stale := len(c.toolTitle["s1"])
	c.mu.Unlock()
	if stale != 0 {
		t.Errorf("tool title tracking must clear with the turn, %d left", stale)
	}
}

// TestACPControlProviderSwitchAckShowsNewProvider: the ack to a coop_provider switch must render
// the NEW lead (provider dropdown + its accounts), not echo the old one — retargeting used to
// happen only at spawn time, so the ack showed the previous provider and the editor's dropdown
// visibly flipped back until the respawn's config_option_update arrived.
func TestACPControlProviderSwitchAckShowsNewProvider(t *testing.T) {
	c := newTestControl(t)
	// A signed-in codex account, so the provider switch is spawnable (selectorSel refuses otherwise).
	codexDir := filepath.Join(c.cfg.ConfigDir, "codex", "profiles", "personal")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Prime the cache as a claude session/new would — its NATIVE model menu is claude's.
	toEd(c, []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s","configOptions":[{"id":"model","type":"select","currentValue":"default","options":[{"value":"opus[1m]","name":"Opus"}]}]}}`))
	handled, resp, _, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":11,"method":"session/set_config_option","params":{"sessionId":"s","configId":"coop_provider","value":"codex"}}`))
	if !handled || !restart {
		t.Fatalf("a provider switch must be handled and restart the box (handled=%v restart=%v)", handled, restart)
	}
	ids, res := configOptionIDs(t, resp)
	var opts []struct {
		ID      string `json:"id"`
		Current string `json:"currentValue"`
	}
	json.Unmarshal(res["configOptions"], &opts)
	for _, o := range opts {
		if o.ID == "coop_provider" && o.Current != "codex" {
			t.Errorf("ack renders provider %q, want codex (the switch already applied)", o.Current)
		}
	}
	// The old lead's NATIVE model menu must NOT survive into the ack — it would list claude
	// models on what is now a codex session; the new box's truth brings the right one.
	if slices.Contains(ids, "model") {
		t.Errorf("ack still carries the previous provider's model dropdown: %v", ids)
	}
	// The per-lead state followed: the next spawn resolves codex with its default account.
	if tgt, _, ok := c.spawnTarget(); !ok || tgt.Provider != "codex" {
		t.Errorf("spawnTarget after the switch = %+v ok=%v, want provider codex", tgt, ok)
	}
	// The respawned box's session/new truth restores the native menu (the new lead's).
	toEd(c, []byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s","configOptions":[{"id":"model","type":"select","currentValue":"gpt-5.5","options":[{"value":"gpt-5.5","name":"gpt-5.5"}]}]}}`))
	if ids2, _ := configOptionIDs(t, c.ackOptions(json.RawMessage("13"), "s")); !slices.Contains(ids2, "model") {
		t.Errorf("after the new box's truth the model dropdown must be back: %v", ids2)
	}
}

// TestACPPresetOwnsToolbar: plain sessions expose the normal trio; a preset exposes only its
// selector because its ladder owns provider, model, effort, account, and roles.
func TestACPPresetOwnsToolbar(t *testing.T) {
	c := newTestControl(t)
	ids := coopIDs(c.coopOptions())
	if len(ids) != 3 || ids[0] != "coop_preset" || ids[1] != "coop_provider" || ids[2] != "coop_account" {
		t.Fatalf("plain lead wants preset+provider+account, got %v", ids)
	}
	c.sel = acpSelection{Preset: "frontier"}
	if ids := coopIDs(c.coopOptions()); !slices.Equal(ids, []string{coopPresetID}) {
		t.Errorf("preset toolbar must contain only coop_preset, got %v", ids)
	}
}

// coopIDs pulls the ordered ids out of a coopOptions() slice.
func coopIDs(opts []json.RawMessage) []string {
	var ids []string
	for _, o := range opts {
		var h struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(o, &h)
		ids = append(ids, h.ID)
	}
	return ids
}

// TestACPControlSnapshotRestore: the selection state survives a supervisor re-exec — snapshot a
// non-default lead/model/set-model, restore into a fresh controller, assert it comes back.
func TestACPControlSnapshotRestore(t *testing.T) {
	dir := t.TempDir()
	c := newACPControl(&config.Config{ConfigDir: dir}, "claude", "opus", dir, acpSelection{}, nil, nil, false)
	c.sel, c.lead, c.model, c.leadUsesSetModel = acpSelection{Provider: "codex"}, "codex", "gpt-5.6-sol", true
	snap := c.snapshot()

	c2 := newACPControl(&config.Config{ConfigDir: dir}, "claude", "opus", dir, acpSelection{}, nil, nil, false)
	c2.restore(snap)
	if c2.sel != (acpSelection{Provider: "codex"}) || c2.lead != "codex" || c2.model != "gpt-5.6-sol" || !c2.leadUsesSetModel {
		t.Errorf("restore mismatch: sel=%+v lead=%q model=%q setModel=%v", c2.sel, c2.lead, c2.model, c2.leadUsesSetModel)
	}
}
