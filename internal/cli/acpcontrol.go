package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

// defaultLimitCooldown is how long a credential is skipped after a rate limit when the provider's
// error gave no explicit reset time — long enough to matter, short enough to come back to.
const defaultLimitCooldown = 5 * time.Minute

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

	accounts []string // the lead's signed-in accounts, for rate-limit auto-rotation (default first)

	mu      sync.Mutex
	sel     string                     // current coop_setup value: "cred:<name>" or "preset:<name>"
	cached  map[string]json.RawMessage // sessionId -> the rewritten configOptions array (for set responses)
	limited map[string]time.Time       // account -> when its rate limit resets (skip until then)
	nextID  int
}

func newACPControl(cfg *config.Config, lead, model, cred string, presets []string) *acpControl {
	sel := "cred:" + cfg.ActiveProfile(lead)
	if cred != "" {
		sel = "cred:" + cred
	}
	return &acpControl{
		lead: lead, model: model, creds: cfg.Profiles(lead), presets: presets,
		accounts: accountsFor(cfg, lead),
		sel:      sel, cached: map[string]json.RawMessage{}, limited: map[string]time.Time{},
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

// toEditor rewrites an agent→editor line. On any object carrying configOptions/modes (a
// session/new|load|resume result or a ConfigOptionUpdate notification), it drops the mode+subagent
// dropdowns and the modes mirror, defaults the model to coop's, and prepends coop's selector. It also
// watches for a rate-limit error and auto-rotates the credential (restart=true) — see maybeRotate.
func (c *acpControl) toEditor(line []byte) (out []byte, restart bool) {
	if rewritten, rotated := c.maybeRotate(line); rotated {
		return rewritten, true
	}
	// Fast path: only session/new|load|resume results and ConfigOptionUpdate notifications carry these,
	// so skip parsing the (often large) prompt/tool-call traffic entirely.
	if !bytes.Contains(line, []byte("configOptions")) && !bytes.Contains(line, []byte(`"modes"`)) {
		return line, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(line, &m) != nil {
		return line, false
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
				return append(out, '\n'), false
			}
		}
	}
	return line, false
}

// maybeRotate implements Step 4 — auto-rotate credentials on a rate limit — reusing the loop's
// detectLimit + account rotation. If this is a CREDENTIAL session (a preset rotates via its own
// models ladder, not here) and the line is a JSON-RPC error whose message trips the rate-limit
// detector, it marks the current account limited, picks the next signed-in account that isn't, points
// the selection at it, and returns (rewritten error, true) so the proxy restarts the box on it —
// the same transparent restart+replay path as a manual switch. Returns (nil,false) to forward the
// line unchanged (not an error, not a limit, or nowhere free to rotate to → the user just waits).
func (c *acpControl) maybeRotate(line []byte) (out []byte, rotated bool) {
	if !bytes.Contains(line, []byte(`"error"`)) {
		return nil, false
	}
	cred, preset := c.selection()
	if cred == "" || preset != "" {
		return nil, false
	}
	var h struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &h) != nil || h.Error == nil || h.Error.Message == "" {
		return nil, false
	}
	now := time.Now()
	hint := detectLimit(h.Error.Message, now)
	if !hint.limited {
		return nil, false
	}
	until := hint.resetAt
	if until.IsZero() {
		until = now.Add(defaultLimitCooldown)
	}
	next := c.nextAccount(cred, until, now)
	if next == "" {
		return nil, false // one account, or all cooling — forward the error; the user waits/retries
	}
	c.mu.Lock()
	c.sel = "cred:" + next
	c.mu.Unlock()
	msg := fmt.Sprintf("coop: %s is rate limited — switched to %s; resend your last message", cred, next)
	return rewriteErrorMessage(line, msg), true
}

// nextAccount marks cur rate-limited until `until`, then returns the next signed-in account (cyclic
// from cur) whose own limit has expired, or "" when there's no other free account right now.
func (c *acpControl) nextAccount(cur string, until, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limited[cur] = until
	n := len(c.accounts)
	start := -1
	for i, a := range c.accounts {
		if a == cur {
			start = i
			break
		}
	}
	for i := 1; i <= n; i++ {
		cand := c.accounts[(start+i)%n]
		if cand == cur {
			continue
		}
		if t, ok := c.limited[cand]; !ok || !t.After(now) {
			return cand
		}
	}
	return ""
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

// rewriteErrorMessage replaces the message of a JSON-RPC error line, preserving its id + code (so the
// editor's request is still answered — with coop's "switched to <account>" note instead of the raw
// provider error). Returns the line unchanged if it isn't a well-formed error object.
func rewriteErrorMessage(line []byte, msg string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(line, &m) != nil {
		return line
	}
	var e map[string]json.RawMessage
	if json.Unmarshal(m["error"], &e) != nil {
		return line
	}
	mb, _ := json.Marshal(msg)
	e["message"] = mb
	eb, err := json.Marshal(e)
	if err != nil {
		return line
	}
	m["error"] = eb
	out, err := json.Marshal(m)
	if err != nil {
		return line
	}
	return append(out, '\n')
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
