package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

// acpPresetNames lists the repo's presets whose lead is the current ACP agent — the SAME-provider
// presets safe to switch into transparently (a different-provider preset would be a provider switch,
// which coop can't do without losing the conversation; see the backlog).
func (a *app) acpPresetNames(repo, lead string) []string {
	var out []string
	for _, name := range preset.List(repo) {
		if p, err := preset.Load(repo, name); err == nil && p.LeadAgent == lead {
			out = append(out, name)
		}
	}
	return out
}

// coopSetupID is coop's own configOption — the FIRST dropdown in the editor toolbar. Its value is
// "cred:<name>" (run on a stored account) or "preset:<name>" (run an orchestration recipe). It's not
// a native adapter option, so the proxy intercepts its session/set_config_option and restarts the
// box on the chosen identity instead of forwarding it to the adapter.
const coopSetupID = "coop_setup"

// stripConfigIDs are the native claude-agent-acp toolbar dropdowns coop removes: the permission-mode
// picker (coop always runs yolo — the box is the sandbox) and the subagent picker (subagents still
// auto-delegate; coop just drops the manual selector). model/effort/fast stay.
var stripConfigIDs = map[string]bool{"mode": true, "agent": true}

// acpControl is coop's control layer over one ACP editor session. It rewrites the toolbar the editor
// sees (force yolo, default the model to coop's, drop mode/subagent, prepend the credential/preset
// selector) and handles selecting a credential/preset by restarting the box on the chosen identity —
// the conversation survives because the transcript sits on a shared, credential-independent store.
type acpControl struct {
	lead    string   // lead agent (claude/codex/gemini)
	model   string   // coop's resolved model for the lead ("" → leave the adapter's default)
	creds   []string // the lead's credentials (accounts), in order
	presets []string // the repo's presets, in order

	mu     sync.Mutex
	sel    string                     // current coop_setup value: "cred:<name>" or "preset:<name>"
	cached map[string]json.RawMessage // sessionId -> the rewritten configOptions array (for set responses)
	nextID int
}

func newACPControl(cfg *config.Config, lead, model, cred string, presets []string) *acpControl {
	sel := "cred:" + cfg.ActiveProfile(lead)
	if cred != "" {
		sel = "cred:" + cred
	}
	return &acpControl{
		lead: lead, model: model, creds: cfg.Profiles(lead), presets: presets,
		sel: sel, cached: map[string]json.RawMessage{},
	}
}

// hooks wires the controller into the acpproxy.
func (c *acpControl) hooks() *acpproxy.Hooks {
	return &acpproxy.Hooks{
		ToEditor:     c.toEditor,
		SessionReady: c.sessionReady,
		FromEditor:   c.fromEditor,
		AutoReply:    c.autoReply,
	}
}

// selection returns the current credential and preset (one is "", the other set) the factory reads
// to build the box for the next (re)spawn.
func (c *acpControl) selection() (cred, preset string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := strings.CutPrefix(c.sel, "cred:"); ok {
		return v, ""
	}
	if v, ok := strings.CutPrefix(c.sel, "preset:"); ok {
		return "", v
	}
	return "", ""
}

// toEditor rewrites an agent→editor line: on any object carrying configOptions/modes (a
// session/new|load|resume result or a ConfigOptionUpdate notification), it drops the mode+subagent
// dropdowns and the modes mirror, defaults the model to coop's, and prepends coop's selector.
func (c *acpControl) toEditor(line []byte) []byte {
	// Fast path: only session/new|load|resume results and ConfigOptionUpdate notifications carry these,
	// so skip parsing the (often large) prompt/tool-call traffic entirely.
	if !bytes.Contains(line, []byte("configOptions")) && !bytes.Contains(line, []byte(`"modes"`)) {
		return line
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(line, &m) != nil {
		return line
	}
	for _, key := range []string{"result", "params"} {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var inner map[string]json.RawMessage
		if json.Unmarshal(raw, &inner) != nil {
			continue
		}
		_, hasCO := inner["configOptions"]
		_, hasModes := inner["modes"]
		if !hasCO && !hasModes {
			continue
		}
		delete(inner, "modes") // coop is always yolo; no permission-mode dropdown
		if hasCO {
			inner["configOptions"] = c.rewriteConfigOptions(inner["configOptions"], sessionIDOf(inner))
		}
		if nb, err := json.Marshal(inner); err == nil {
			m[key] = nb
			if out, err := json.Marshal(m); err == nil {
				return append(out, '\n')
			}
		}
	}
	return line
}

type acpOption struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// permOption is one choice in a session/request_permission request (ACP kinds: allow_once,
// allow_always, reject_once, reject_always).
type permOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

// rewriteConfigOptions drops the stripped dropdowns, retargets the model to coop's (when it's one of
// the adapter's offered values), prepends coop's credential/preset selector, and caches the result
// per session so a set_config_option response can echo the full set. Native options pass through as
// raw JSON so any fields coop doesn't model survive.
func (c *acpControl) rewriteConfigOptions(raw json.RawMessage, sid string) json.RawMessage {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return raw
	}
	out := []json.RawMessage{c.setupOption()}
	for _, item := range arr {
		var head struct {
			ID      string      `json:"id"`
			Options []acpOption `json:"options"`
		}
		_ = json.Unmarshal(item, &head)
		if stripConfigIDs[head.ID] {
			continue
		}
		if head.ID == "model" && c.model != "" && optionHasValue(head.Options, c.model) {
			item = withField(item, "currentValue", c.model) // default to coop's model; still switchable
		}
		out = append(out, item)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	if sid != "" {
		c.mu.Lock()
		c.cached[sid] = b
		c.mu.Unlock()
	}
	return b
}

// setupOption builds coop's first dropdown (the lead's credentials + the repo's presets) as JSON.
func (c *acpControl) setupOption() json.RawMessage {
	c.mu.Lock()
	cur := c.sel
	c.mu.Unlock()
	opts := make([]acpOption, 0, len(c.creds)+len(c.presets))
	for _, cr := range c.creds {
		opts = append(opts, acpOption{Value: "cred:" + cr, Name: cr, Description: "account"})
	}
	for _, ps := range c.presets {
		opts = append(opts, acpOption{Value: "preset:" + ps, Name: ps, Description: "preset"})
	}
	co := map[string]any{
		"id": coopSetupID, "name": "coop", "description": "Run on a credential (account) or a preset (recipe)",
		"category": "coop", "type": "select", "currentValue": cur, "options": opts,
	}
	b, _ := json.Marshal(co)
	return b
}

// withField sets one top-level string field on a JSON object, preserving every other field.
func withField(obj json.RawMessage, key, value string) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(obj, &m) != nil {
		return obj
	}
	vb, _ := json.Marshal(value)
	m[key] = vb
	if b, err := json.Marshal(m); err == nil {
		return b
	}
	return obj
}

// sessionReady returns the per-session force-sets coop injects to the adapter after a session is
// (re)established, re-applied on every restart (the adapter resets them each launch). Yolo itself is
// enforced provider-agnostically in autoReply (approve every permission request), so this only sets
// what a specific adapter exposes as a config option: claude's mode=bypassPermissions (so its toolbar
// also reflects yolo and it skips the permission round-trips), and coop's model where supported
// (claude, codex; a no-op the adapter rejects and the proxy swallows otherwise).
func (c *acpControl) sessionReady(sid string) [][]byte {
	var msgs [][]byte
	if c.lead == "claude" {
		msgs = append(msgs, c.setConfig(sid, "mode", "bypassPermissions"))
	}
	if c.model != "" {
		msgs = append(msgs, c.setConfig(sid, "model", c.model))
	}
	return msgs
}

// autoReply is coop's yolo mechanism: it approves every session/request_permission by selecting the
// adapter's allow option, replying straight to the adapter so the editor never sees a prompt — for
// every provider, whatever its own permission settings. Any other agent→editor request passes through
// (forward=true) so the editor still services fs/terminal capabilities as normal.
func (c *acpControl) autoReply(line []byte) (reply []byte, forward bool) {
	var h struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Options []permOption `json:"options"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &h) != nil || h.Method != "session/request_permission" {
		return nil, true
	}
	opt := chooseAllow(h.Params.Options)
	var outcome map[string]any
	if opt != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": opt}
	} else {
		outcome = map[string]any{"outcome": "cancelled"} // no option offered: don't fabricate an id
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": h.ID, "result": map[string]any{"outcome": outcome}}
	b, _ := json.Marshal(resp)
	return append(b, '\n'), false
}

// setConfig builds a session/set_config_option request to the adapter, with an InjectPrefix id so the
// proxy swallows its response.
func (c *acpControl) setConfig(sid, id, value string) []byte {
	c.mu.Lock()
	c.nextID++
	n := c.nextID
	c.mu.Unlock()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      acpproxy.InjectPrefix + itoa(n),
		"method":  "session/set_config_option",
		"params":  map[string]any{"sessionId": sid, "configId": id, "value": value},
	}
	b, _ := json.Marshal(req)
	return append(b, '\n')
}

// fromEditor intercepts the editor's set of coop's own selector: it updates the selection and asks
// the proxy to restart the box on the new credential/preset, replying to the editor itself (the
// adapter never sees coop_setup). Native option sets (model/effort/fast) return handled=false so
// they pass through to the adapter unchanged.
func (c *acpControl) fromEditor(line []byte) (handled bool, resp []byte, restart bool) {
	var h struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			ConfigID  string `json:"configId"`
			Value     string `json:"value"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &h) != nil || h.Method != "session/set_config_option" || h.Params.ConfigID != coopSetupID {
		return false, nil, false
	}
	c.mu.Lock()
	c.sel = h.Params.Value
	cached := c.cached[h.Params.SessionID]
	c.mu.Unlock()
	// Reply with the full option set (coop's selector now showing the new value) so the editor's UI
	// stays in sync; then restart the box on the new identity.
	result := map[string]json.RawMessage{}
	if len(cached) > 0 {
		result["configOptions"] = cached
	}
	out := map[string]any{"jsonrpc": "2.0", "id": h.ID, "result": result}
	b, _ := json.Marshal(out)
	return true, append(b, '\n'), true
}

// chooseAllow picks the "approve" option from a request_permission request. ACP kinds are
// allow_always / allow_once / reject_always / reject_once; prefer a lasting allow, then a one-shot
// allow, then anything allow-ish by kind or id, then the first offered option. Returns "" only when
// no options are offered.
func chooseAllow(opts []permOption) string {
	for _, want := range []string{"allow_always", "allow_once"} {
		for _, o := range opts {
			if o.Kind == want {
				return o.OptionID
			}
		}
	}
	for _, o := range opts {
		if strings.Contains(o.Kind, "allow") || strings.Contains(o.OptionID, "allow") {
			return o.OptionID
		}
	}
	if len(opts) > 0 {
		return opts[0].OptionID
	}
	return ""
}

func optionHasValue(opts []acpOption, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

func sessionIDOf(inner map[string]json.RawMessage) string {
	raw, ok := inner["sessionId"]
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
