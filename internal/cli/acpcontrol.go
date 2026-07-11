package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

// defaultLimitCooldown is how long a credential is skipped after a rate limit when the provider's
// error gave no explicit reset time — long enough to matter, short enough to come back to.
const defaultLimitCooldown = 5 * time.Minute

// maxACPLimitWaits caps CONSECUTIVE all-limited waits per session — the ACP analog of the
// loop's maxLimitWaits, sized for an interactive editor: 12 default cooldowns is about an
// hour of respawn churn with no completed turn, after which the real limit error reaches
// the editor instead of another silent "waiting…" cycle. A free rotation or a completed
// turn breaks the chain.
const maxACPLimitWaits = 12

// acpPresetNames lists the repo's loadable presets for the ACP selector — ANY lead: switching to
// a different-provider preset is a provider switch, which the proxy now survives (the session is
// re-created and the conversation carried best-effort as a text preamble; see spawnTarget).
func (a *app) acpPresetNames(repo string) []string {
	globalDir := a.cfg.GlobalPresetsDir()
	var out []string
	for _, name := range preset.List(repo, globalDir) {
		if _, err := preset.Load(repo, globalDir, name); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// coop's own configOptions — the FIRST dropdowns in the editor toolbar, mirroring the target
// grammar: Provider (who runs — switching re-creates the thread, context carried best-effort),
// Account (the lead's login — a transparent switch), and Preset (the orchestration recipe).
// They render three ways but share ONE selection underneath (c.sel): the last-changed dropdown
// wins and the others refresh to the effective state. None is a native adapter option, so the
// proxy intercepts their session/set_config_option and restarts the box on the chosen identity.
const (
	coopProviderID = "coop_provider"
	coopAccountID  = "coop_account"
	coopPresetID   = "coop_preset"
	// coopSetupID is the RETIRED single mixed dropdown ("cred:<name>" / "preset:<name>" /
	// "agent:<name>" values). Still ACCEPTED on set: an editor's persisted
	// default_config_options may replay it at startup, and breaking that would strand a
	// running config. Never rendered anymore.
	coopSetupID = "coop_setup"
)

// coopOwnedIDs are the selector ids coop rebuilds on every refresh (the retired one included,
// so a stale cached array can't keep rendering it).
var coopOwnedIDs = map[string]bool{coopProviderID: true, coopAccountID: true, coopPresetID: true, coopSetupID: true}

// stripConfigIDs are the native claude-agent-acp toolbar dropdowns coop removes: the permission-mode
// picker (coop always runs yolo — the box is the sandbox) and the subagent picker (subagents still
// auto-delegate; coop just drops the manual selector). model/effort/fast stay.
var stripConfigIDs = map[string]bool{"mode": true, "agent": true}

// acpControl is coop's control layer over one ACP editor session. It rewrites the toolbar the editor
// sees (force yolo, default the model to coop's, drop mode/subagent, prepend the credential/preset
// selector) and handles selecting a credential/preset by restarting the box on the chosen identity —
// the conversation survives because the transcript sits on a shared, credential-independent store.
type acpControl struct {
	cfg     *config.Config // for expanding a selected preset's model ladder into rotation targets
	repo    string         // repo root, to load a preset by name on a coop_setup switch
	lead    string         // the CURRENT lead agent — re-derived on a provider switch (see retarget)
	model   string         // coop's resolved model for the lead ("" → leave the adapter's default)
	creds   []string       // the lead's credentials (accounts), in order
	presets []string       // the repo's presets, in order
	fusion  bool           // a fusion governor session — the selector offers no provider switch

	accounts []string // the lead's signed-in accounts, for rate-limit auto-rotation (default first)

	mu     sync.Mutex
	sel    string                     // current coop_setup value: "cred:<name>" or "preset:<name>"
	cached map[string]json.RawMessage // sessionId -> the rewritten configOptions array (for set responses)

	// leadUsesSetModel latches true once a session/new result proves this lead exposes its models via a
	// `models` field with no native `model` configOption (gemini), so coop synthesized the dropdown. It
	// then routes an editor `model` set to session/set_model (fromEditor) and re-applies the chosen model
	// after a box swap (sessionReady). Stays false for adapters with a native model option (claude, codex).
	leadUsesSetModel bool
	limited          map[string]time.Time // account -> when its rate limit resets (skip until then)
	nextID           int

	// Preset rate-limit failover: a preset session rotates the lead's model ladder (fable→opus→…),
	// unlike a credential session (which rotates accounts on one model). rot is the active preset's
	// rotation (nil for a credential session); rotPreset names the preset it was built for, so it's
	// rebuilt when the coop_setup selection moves to a different preset.
	rot       *rotation
	rotPreset string

	// Transparent rate-limit resend: correlate a rate-limit error (which carries only a request id)
	// back to its session, and re-send that prompt once the box is back on a fresh credential.
	promptSession map[string]string // in-flight session/prompt: request id -> editor sessionId
	lastPrompt    map[string][]byte // editor sessionId -> its latest prompt line (what to resend)
	resend        map[string]bool   // editor sessionId -> re-send the last prompt after the next restart
	heldChunk     map[string][]byte // editor sessionId -> a buffered rate-limit notice awaiting the turn's outcome
	waits         map[string]int    // editor sessionId -> CONSECUTIVE all-limited waits (see maxACPLimitWaits)

	serveURLs []string        // published-port lines to show the editor (e.g. "box :5173 → http://localhost:24187")
	reported  map[string]bool // editor sessionId -> the serve URLs were already announced in this session

	// Best-effort conversation carry across a session re-create (a provider switch, or a lost
	// transcript): coop retains a budgeted plain-text history per session — it already sees both
	// directions — and when the proxy re-creates a session (SessionRecreated), the next outgoing
	// prompt is wrapped with a labeled preamble. Approximate by design: message text plus one-line
	// tool NARRATION ("[tool] title — status"); tool payloads are excluded — results dominate
	// transcripts and go stale, the narrative is what carries.
	carryBytes   int                          // per-session history budget (COOP_ACP_CARRY_TOKENS × ~4 bytes/token)
	history      map[string]*sessHistory      // editor sessionId -> its budgeted conversation history
	turnText     map[string][]byte            // editor sessionId -> the in-progress assistant turn's narrative (tail-bounded)
	toolTitle    map[string]map[string]string // editor sessionId -> toolCallId -> title (until its terminal update)
	needPreamble map[string]bool              // editor sessionId -> wrap the next outgoing prompt with the history
}

// sessHistory is one session's carried conversation: (user, assistant) texts in order, plus the
// provider it ran on (for the preamble's "carried from X" label — last writer wins across a
// switch chain). size tracks the entries' total bytes for the eviction cap; evicted notes that
// older context was dropped, so the preamble says so instead of implying completeness.
type sessHistory struct {
	lead    string
	entries []histEntry
	size    int
	evicted bool
}

type histEntry struct{ role, text string }

// historyEntryBytes caps one entry's text. A user prompt keeps its HEAD (the ask leads); an
// assistant turn keeps its TAIL (conclusions land last). Plain bytes, not tokens — close
// enough for a best-effort carry. The per-session budget is cfg-owned (COOP_ACP_CARRY_TOKENS).
const historyEntryBytes = 16 << 10

func newACPControl(cfg *config.Config, lead, model, cred, repo string, presets, serveURLs []string, fusion bool) *acpControl {
	sel := "cred:" + cfg.ActiveProfile(lead)
	if cred != "" {
		sel = "cred:" + cred
	}
	return &acpControl{
		cfg: cfg, repo: repo, fusion: fusion,
		lead: lead, model: model, creds: cfg.Profiles(lead), presets: presets,
		accounts:      accountsFor(cfg, lead),
		sel:           sel,
		cached:        map[string]json.RawMessage{},
		limited:       map[string]time.Time{},
		promptSession: map[string]string{},
		lastPrompt:    map[string][]byte{},
		resend:        map[string]bool{},
		heldChunk:     map[string][]byte{},
		waits:         map[string]int{},
		serveURLs:     serveURLs,
		reported:      map[string]bool{},
		carryBytes:    cfg.ACPCarryBytes(),
		history:       map[string]*sessHistory{},
		turnText:      map[string][]byte{},
		toolTitle:     map[string]map[string]string{},
		needPreamble:  map[string]bool{},
	}
}

// hooks wires the controller into the acpproxy.
func (c *acpControl) hooks() *acpproxy.Hooks {
	return &acpproxy.Hooks{
		ToEditor:         c.toEditor,
		SessionReady:     c.sessionReady,
		FromEditor:       c.fromEditor,
		AutoReply:        c.autoReply,
		ResumePrompt:     c.resumePrompt,
		SessionRecreated: c.sessionRecreated,
	}
}

// sessionRecreated marks a session whose conversation did not carry across a restart (the proxy
// re-created it fresh — a provider switch, or a lost transcript): the next outgoing prompt gets
// the best-effort history preamble, whether that's a transparent resend or the editor's next message.
func (c *acpControl) sessionRecreated(session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.needPreamble[session] = true
}

// resumePrompt returns the prompt to transparently re-send for a session once its box is back after a
// rate-limit rotation/wait, or nil. One-shot: the flag is cleared so a later restart doesn't re-send.
//
// Known, accepted artifact (v3 waiver): the box that died on the limit usually persisted this prompt
// as a dangling user turn in the adapter's OWN session transcript before erroring, so after the
// replayed session/load the resend leaves that stored transcript with the user message twice.
// Removing it would mean racing surgery on the adapter's private session JSONL for zero functional
// gain: the duplicate never renders in the editor (the resend reuses the editor's original request,
// and replay drops the adapter's history re-stream — TestProxyReplayDropsHistoryRestream), and the
// model treats an adjacent identical user message benignly, so multi-turn use is unaffected.
func (c *acpControl) resumePrompt(session string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resend[session] {
		return nil
	}
	delete(c.resend, session)
	prompt := c.lastPrompt[session]
	// The restart that triggered this resend may have re-created the session (a cross-provider
	// rotation): the resent prompt is then the first into a fresh thread, so it carries the
	// history preamble — same one-shot flag the editor's-next-prompt path consumes.
	if c.needPreamble[session] && len(prompt) > 0 {
		delete(c.needPreamble, session)
		if pre := c.preambleLocked(session); pre != "" {
			prompt = wrapPromptLine(prompt, pre)
			c.lastPrompt[session] = prompt
		}
	}
	return prompt
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

// currentModel is coop's chosen model for the lead. Read under the lock because a set of coop's
// synthesized model dropdown (gemini) mutates it from the editor goroutine while the box→editor
// goroutine reads it to render the toolbar.
func (c *acpControl) currentModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.model
}

// presetRotation returns the active preset's model-ladder rotation, built once per preset from the
// preset's lead ladder (expandLadder — the SAME targets `coop loop` cycles). Returns nil for a
// credential session, or when the ladder can't be expanded (no signed-in account, or the preset won't
// load) — the caller then falls back to the preset's own first entry with no failover. The rotation is
// cached and rebuilt only when the selected preset changes, so its cursor + per-target limits persist
// across respawns.
func (c *acpControl) presetRotation() *rotation {
	_, psName := c.selection()
	if psName == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rot != nil && c.rotPreset == psName {
		return c.rot
	}
	c.rotPreset, c.rot = psName, nil
	p, err := preset.Load(c.repo, c.cfg.GlobalPresetsDir(), psName)
	if err != nil {
		return nil
	}
	// The whole ladder, cross-provider rungs included: the respawn env carries a full target,
	// so a rung on another provider swaps the lead (the proxy re-creates the session there and
	// the conversation is carried best-effort as a text preamble).
	targets, err := expandLadder(c.cfg, p.LeadAgent, p.LeadLadder)
	if err != nil || len(targets) == 0 {
		return nil
	}
	c.rot = newRotation(targets)
	return c.rot
}

// spawnTarget resolves what the NEXT box spawn runs — the full target (provider, model,
// account) the factory exports as COOP_ACP_TARGET, plus the preset whose roles mount — and
// re-derives the control's per-lead state when the provider changes hands (a manual provider
// switch, a different-lead preset, or a cross-provider preset rung). ok=false only when the
// selection is unrecognizable (the factory then spawns on the launch args alone).
func (c *acpControl) spawnTarget() (t agents.Target, presetName string, ok bool) {
	_, psName := c.selection()
	var rung *agents.Target
	if psName != "" {
		if rot := c.presetRotation(); rot != nil { // locks internally — resolve before taking c.mu
			r := rot.active()
			rung = &r
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case strings.HasPrefix(c.sel, "cred:"):
		// A credential switch stays on the current lead and carries coop's model pick, so the
		// respawned adapter keeps it even where the model rides the spawn command (gemini).
		t = agents.Target{Provider: c.lead, Model: c.model}
		if v := strings.TrimPrefix(c.sel, "cred:"); v != "" {
			t.Accounts = []string{v}
		}
	case strings.HasPrefix(c.sel, "agent:"):
		// A provider switch: that provider's marked default account and default model — the old
		// lead's model id means nothing to it.
		t = agents.Target{Provider: strings.TrimPrefix(c.sel, "agent:")}
	case psName != "":
		if rung != nil {
			t = *rung // the active ladder rung — provider included (a cross-provider rung swaps the lead)
		} else {
			// No rotation (no signed-in rungs / unloadable): spawn the preset's lead bare; the
			// inner's applyPreset then uses the preset's own first entry.
			t = agents.Target{Provider: c.presetLeadLocked(psName)}
		}
	default:
		return agents.Target{}, "", false
	}
	c.retargetLocked(t.Provider)
	return t, psName, true
}

// presetLeadLocked resolves a preset's lead provider for the no-rotation spawn fallback,
// falling back to the current lead when the preset won't load (the inner will surface the
// real load error). Called with c.mu held; preset.Load is pure file reads.
func (c *acpControl) presetLeadLocked(name string) string {
	if p, err := preset.Load(c.repo, c.cfg.GlobalPresetsDir(), name); err == nil {
		return p.LeadAgent
	}
	return c.lead
}

// retargetLocked re-derives the per-lead state when the next spawn's provider differs from the
// current lead: the selector's credential list, the auto-rotation accounts, and the set-model
// latch all belong to the NEW lead, and coop's remembered model pick dies (its id is the old
// provider's). Rate-limit cooldowns keyed by account name are left alone — stale entries for the
// old lead expire on their own. Called with c.mu held.
func (c *acpControl) retargetLocked(provider string) {
	if provider == "" || provider == c.lead {
		return
	}
	c.lead = provider
	c.creds = c.cfg.Profiles(provider)
	c.accounts = accountsFor(c.cfg, provider)
	c.model = ""
	c.leadUsesSetModel = false
}

// waitForReset blocks until a rate-limited credential's reset passes (or ctx is done), so a respawn the
// wait-for-reset path pointed at a still-cooling account only starts once it's usable. A no-op for an
// account that isn't limited — the common case, including a normal rotation to a free account.
func (c *acpControl) waitForReset(ctx context.Context, cred string) {
	c.mu.Lock()
	until := c.limited[cred]
	c.mu.Unlock()
	sleepUntilReset(ctx, until, "credential "+cred)
}

// waitForPresetRung blocks until the active preset rung's rate limit resets (the all-rungs-limited wait
// path — rotatePreset advanced the cursor to the soonest-resetting rung), mirroring waitForReset for a
// credential. A no-op when the rung is already free (a normal rotation to a fresh rung).
func (c *acpControl) waitForPresetRung(ctx context.Context) {
	c.mu.Lock()
	var until time.Time
	label := "preset rung"
	if c.rot != nil {
		t := c.rot.active()
		until, label = c.rot.limited[t.String()], "preset rung "+t.String()
	}
	c.mu.Unlock()
	sleepUntilReset(ctx, until, label)
}

// sleepUntilReset blocks until `until` passes (capped at limitMaxWait) or ctx is done; a no-op when
// until is zero or already past. Shared by the credential and preset wait-for-reset paths, so a respawn
// pointed at a still-cooling target only starts once it's usable.
func sleepUntilReset(ctx context.Context, until time.Time, label string) {
	now := time.Now()
	d := until.Sub(now)
	if d <= 0 {
		return
	}
	deadline := until
	if d > limitMaxWait { // bound a far-future reset so a bad value can't strand the respawn
		deadline, d = now.Add(limitMaxWait), limitMaxWait
	}
	acpproxy.Trace("waiting %s for %s to reset before spawning", d.Round(time.Second), label)
	// Re-check against the wall clock on short ticks (shared with the loop's sleepForLimit): a
	// laptop suspend freezes the monotonic clock, so a single long timer would resume on wake still
	// counting the closed time and over-wait past the reset.
	waitUntilWall(deadline, limitTickCap, time.Now, ctx.Done(), nil)
}

// toEditor rewrites an agent→editor line. On any object carrying configOptions/modes (a
// session/new|load|resume result or a ConfigOptionUpdate notification), it drops the mode+subagent
// dropdowns and the modes mirror, defaults the model to coop's, and prepends coop's selector. It also
// watches for a rate-limit error and auto-rotates the credential (restart=true) — see maybeRotate.
func (c *acpControl) toEditor(line []byte) (out []byte, restart bool) {
	// A rate-limit error → rotate to a free account (or wait for the nearest reset) and re-send the
	// prompt transparently; maybeRotate also drops any buffered rate-limit notice for that turn.
	if rewritten, rotated := c.maybeRotate(line); rotated {
		return rewritten, true
	}
	// The carried-history capture: assistant chunks accumulate, a prompt's terminal response
	// flushes the turn. After maybeRotate (a rate-limited turn resends — don't flush its partial),
	// before chunkGate (which forgets the terminal's prompt→session mapping).
	c.captureTurn(line)
	// Buffer a rate-limit notice chunk until the turn's outcome is known — suppressed if the turn then
	// rate-limits (a seamless resend), flushed otherwise. This never drops a legit chunk that merely
	// mentions "rate limit"/"quota"/429, because a chunk is only dropped when a rate-limit error follows.
	if hold, flush := c.chunkGate(line); hold {
		return nil, false
	} else if flush != nil {
		out = append(flush, c.rewriteToEditor(line)...)
	} else {
		out = c.rewriteToEditor(line)
	}
	// Announce this repo's published ports once, when a session is established (its session/new result).
	if notice := c.serveNoticeFor(line); notice != nil {
		out = append(out, notice...)
	}
	return out, false
}

// serveNoticeFor returns a one-shot-per-session message listing the published-port URLs when line is a
// session/new result (which carries both a sessionId and configOptions), or nil. The URLs are stable,
// so once per session is enough; a box restart's session/load result is swallowed by replay and never
// reaches here, so it won't re-announce.
func (c *acpControl) serveNoticeFor(line []byte) []byte {
	if len(c.serveURLs) == 0 {
		return nil
	}
	var m struct {
		Result *struct {
			SessionID     string          `json:"sessionId"`
			ConfigOptions json.RawMessage `json:"configOptions"`
		} `json:"result"`
	}
	if json.Unmarshal(line, &m) != nil || m.Result == nil || m.Result.SessionID == "" || len(m.Result.ConfigOptions) == 0 {
		return nil
	}
	sid := m.Result.SessionID
	c.mu.Lock()
	if c.reported[sid] {
		c.mu.Unlock()
		return nil
	}
	c.reported[sid] = true
	c.nextID++
	n := c.nextID
	c.mu.Unlock()
	text := "🌐 coop is publishing this repo's ports:\n" + strings.Join(c.serveURLs, "\n")
	upd := map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sid,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
				"messageId":     fmt.Sprintf("coop-serve-%d", n),
			},
		},
	}
	b, _ := json.Marshal(upd)
	return append(b, '\n')
}

// rewriteToEditor applies coop's toolbar rewrite: on any object carrying configOptions/modes (a
// session/new|load|resume result or a config_option_update) it drops the mode+subagent dropdowns and the
// modes mirror, defaults the model to coop's, and prepends coop's selector. On a session result that
// carries NO configOptions (the gemini/codex adapters, unlike claude-agent-acp) it still injects coop's
// selector, so the credential/preset switcher appears for every agent. Other lines pass through.
func (c *acpControl) rewriteToEditor(line []byte) []byte {
	// Fast path: only session/new|load|resume results (a result WITH a sessionId) and config_option_update
	// notifications carry a toolbar, so skip parsing the (often large) prompt/tool-call traffic entirely.
	isSessionResult := bytes.Contains(line, []byte(`"sessionId"`)) && bytes.Contains(line, []byte(`"result"`))
	if !isSessionResult && !bytes.Contains(line, []byte("configOptions")) && !bytes.Contains(line, []byte(`"modes"`)) {
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
		// configOptions ride two shapes: directly on a session/new|load|resume RESULT (result.configOptions,
		// beside a modes mirror), or nested in a config_option_update NOTIFICATION (params.update.
		// configOptions) — which the adapter pushes on a mid-session change AND coop's replay rebuilds after
		// a box swap. Rewrite wherever they sit, so coop's toolbar (coop_setup + stripped mode/agent + model
		// retarget) survives both; missing the nested shape is what dropped the coop dropdown after a switch.
		sid := sessionIDOf(inner)
		changed := false
		if _, hasModes := inner["modes"]; hasModes {
			delete(inner, "modes") // coop is always yolo; no permission-mode dropdown
			changed = true
		}
		if _, hasCO := inner["configOptions"]; hasCO {
			inner["configOptions"] = c.rewriteConfigOptions(inner["configOptions"], inner["models"], sid)
			changed = true
		} else if key == "result" && sid != "" {
			// A session/new|load|resume RESULT with no configOptions — the gemini adapter doesn't emit the
			// claude-agent-acp toolbar. coop still owns the toolbar, so synthesize one from an empty set
			// (rewriteConfigOptions prepends coop_setup, plus a model select when the result carries a
			// `models` field); without this the credential/preset switcher never appears for those agents
			// (the reported "gemini shows no dropdowns at all").
			inner["configOptions"] = c.rewriteConfigOptions(json.RawMessage("[]"), inner["models"], sid)
			changed = true
		} else if rewrote := c.rewriteUpdateConfigOptions(inner["update"], sid); rewrote != nil {
			inner["update"] = rewrote
			changed = true
		}
		if !changed {
			continue
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

// rewriteUpdateConfigOptions rewrites a session/update notification's nested update.configOptions —
// the config_option_update shape the adapter pushes on a mid-session change, and the one coop's replay
// rebuilds after a box swap. Returns the re-marshalled update object, or nil when there's no update or
// it carries no configOptions (the caller then leaves the line untouched). Without this, a
// config_option_update bypasses coop's toolbar rewrite and the coop_setup dropdown vanishes after a switch.
func (c *acpControl) rewriteUpdateConfigOptions(update json.RawMessage, sid string) json.RawMessage {
	if len(update) == 0 {
		return nil
	}
	var u map[string]json.RawMessage
	if json.Unmarshal(update, &u) != nil {
		return nil
	}
	if _, ok := u["configOptions"]; !ok {
		return nil
	}
	// A config_option_update carries only the option array — no models field — so pass nil; the model
	// select, if any, was already synthesized at session/new and rides in the cached set.
	u["configOptions"] = c.rewriteConfigOptions(u["configOptions"], nil, sid)
	nb, err := json.Marshal(u)
	if err != nil {
		return nil
	}
	return nb
}

// maybeRotate handles a rate-limit error transparently, reusing the loop's detectLimit. A credential
// session rotates ACCOUNTS on the fixed model; a preset session rotates the lead's MODEL LADDER
// (fable→opus→…, via rotatePreset) — the step a persistent ACP session otherwise never takes, so a
// per-model limit isn't a dead end. Either way it correlates the error back to the prompt that
// triggered it and, if a free target exists, swaps to it and flags the prompt for an automatic re-send
// (returns nil,true — the error is swallowed, the turn completes on the new target); if none is free it
// points at the one that resets soonest, returns a "waiting" status (true, the factory blocks until the
// reset), and flags the same re-send. Falls back to the switch-and-ask-to-resend note (or forwarding the
// error) when it can't identify the prompt. Returns (nil,false) to leave the line untouched.
func (c *acpControl) maybeRotate(line []byte) (out []byte, rotated bool) {
	if !bytes.Contains(line, []byte(`"error"`)) {
		return nil, false
	}
	cred, preset := c.selection()
	if cred == "" && preset == "" {
		return nil, false
	}
	var h struct {
		ID    json.RawMessage `json:"id"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &h) != nil || len(h.Error) == 0 {
		return nil, false
	}
	now := time.Now()
	hint := acpErrorLimitHint(h.Error, now, acpRateSignals(c.lead))
	if !hint.limited || hint.outputLimited {
		return nil, false
	}
	until := hint.resetAt
	if until.IsZero() {
		until = now.Add(defaultLimitCooldown)
	}
	// Correlate the error (which carries only a request id) back to its session, so we can re-send that
	// prompt transparently. Drop the buffered rate-limit notice for this turn either way (never flush it).
	c.mu.Lock()
	session := c.promptSession[string(h.ID)]
	delete(c.promptSession, string(h.ID))
	canResend := session != "" && c.lastPrompt[session] != nil
	if session != "" {
		delete(c.heldChunk, session)
	}
	c.mu.Unlock()

	// A preset session rotates its model ladder (a rung is a model+account); a credential session
	// rotates accounts on the fixed model (below).
	if preset != "" {
		return c.rotatePreset(session, canResend, until, now)
	}

	next := c.nextAccount(cred, until, now)
	if next != "" {
		c.clearWait(session) // a free rotation breaks the consecutive-wait chain
		c.mu.Lock()
		c.sel = "cred:" + next
		if canResend {
			c.resend[session] = true
		}
		c.mu.Unlock()
		if canResend {
			acpproxy.Trace("rate limit on %s: rotating to %s + auto-resending", cred, next)
			// Swallow the error, move the toolbar dropdown to the new credential, restart on it, and
			// re-send after replay — the config_option_update is the only thing the editor sees.
			return c.configOptionUpdate(session), true
		}
		// Couldn't identify the prompt — fall back to switching + asking the user to resend.
		return rewriteErrorMessage(line, fmt.Sprintf("coop: %s is rate limited — switched to %s; resend your last message", cred, next)), true
	}

	// Nothing free right now: wait for the nearest reset, then re-send on that account. Needs a known
	// reset and a captured prompt; otherwise forward the error so the user can wait/retry themselves.
	acct, at := c.nearestReset()
	if acct == "" || !canResend {
		return nil, false
	}
	if c.bumpWait(session) > maxACPLimitWaits {
		// Enough silent respawn churn with no completed turn — hand the user the truth.
		c.clearWait(session)
		acpproxy.Trace("acp: %d consecutive all-limited waits — forwarding the limit error", maxACPLimitWaits)
		return nil, false
	}
	c.mu.Lock()
	c.sel = "cred:" + acct
	c.resend[session] = true
	c.mu.Unlock()
	acpproxy.Trace("all accounts rate limited: waiting for %s until %s + auto-resending", acct, at.Format(time.RFC3339))
	// Move the dropdown to the account we'll resume on, then the "waiting…" status line.
	return append(c.configOptionUpdate(session), c.waitStatus(session, acct, at, now)...), true
}

// rotatePreset advances the active preset's model ladder on a rate limit: mark the current rung
// limited, switch to the next free rung (a different model and/or account) and re-send; or, when every
// rung is limited, point at the soonest-resetting rung and return a "waiting" status (the factory
// blocks on it before respawning). Forwards the raw error (nil,false) when the preset has a single rung
// — nothing to fail over to — or no prompt was captured to re-send. The box respawns on the new rung
// because the factory reads presetTarget() (= rot.active()); the toolbar's model dropdown catches up
// via the replay's config_option_update, while coop_setup stays on the preset.
func (c *acpControl) rotatePreset(session string, canResend bool, until, now time.Time) (out []byte, rotated bool) {
	rot := c.presetRotation()
	if rot == nil || !rot.rotates() || !canResend {
		return nil, false
	}
	c.mu.Lock()
	prev := rot.active()
	sleep, resetAt := rot.onLimit(until, 0, now)
	next := rot.active()
	c.mu.Unlock()
	if sleep <= 0 {
		c.clearWait(session) // a free rung breaks the consecutive-wait chain
		c.mu.Lock()
		c.resend[session] = true
		c.mu.Unlock()
		acpproxy.Trace("preset rate limit on %s: rotating to %s + auto-resending", prev, next)
		return c.configOptionUpdate(session), true
	}
	if c.bumpWait(session) > maxACPLimitWaits {
		c.clearWait(session)
		acpproxy.Trace("preset: %d consecutive all-limited waits — forwarding the limit error", maxACPLimitWaits)
		return nil, false
	}
	c.mu.Lock()
	c.resend[session] = true
	c.mu.Unlock()
	acpproxy.Trace("preset: all rungs rate limited — waiting for %s until %s + auto-resending", next, resetAt.Format(time.RFC3339))
	return append(c.configOptionUpdate(session), c.waitStatus(session, next.Account(), resetAt, now)...), true
}

// configOptionUpdate builds an ACP config_option_update notification (session/update carrying the full
// configOptions) with coop_setup's currentValue refreshed to the current selection — so the editor's
// toolbar dropdown reflects an auto-switch coop made (a rate-limit rotation/wait), just as a manual
// switch's ack does. Falls back to just coop_setup if this session's options weren't cached.
func (c *acpControl) configOptionUpdate(session string) []byte {
	c.mu.Lock()
	cached := c.cached[session]
	c.mu.Unlock()
	upd := map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": session,
			"update": map[string]any{
				"sessionUpdate": "config_option_update",
				"configOptions": json.RawMessage(c.refreshSetup(cached)),
			},
		},
	}
	b, _ := json.Marshal(upd)
	return append(b, '\n')
}

// nearestReset returns the signed-in account whose rate limit resets soonest (and when), or "" if none
// are marked limited. Used when no account is free right now: coop waits for this one, then re-sends.
func (c *acpControl) nearestReset() (account string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.accounts {
		t, ok := c.limited[a]
		if !ok {
			continue
		}
		if at.IsZero() || t.Before(at) {
			account, at = a, t
		}
	}
	return account, at
}

// waitStatus builds the one status line the editor shows while coop waits for a reset (no live
// countdown — the absolute time says when it resumes). It carries the editor's session id so the
// message lands in the right thread, and a coop messageId so the editor renders it.
func (c *acpControl) waitStatus(session, account string, at, now time.Time) []byte {
	c.mu.Lock()
	c.nextID++
	n := c.nextID
	c.mu.Unlock()
	text := fmt.Sprintf("Waiting for a reset on credential %s in %s (at %s) — your message will send automatically.",
		account, formatWait(at.Sub(now)), at.Local().Format("Mon 15:04 MST"))
	upd := map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": session,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
				"messageId":     fmt.Sprintf("coop-wait-%d", n),
			},
		},
	}
	b, _ := json.Marshal(upd)
	return append(b, '\n')
}

// formatWait renders a wait as MM:SS, or Hh MMm once it's an hour or more.
func formatWait(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", d/time.Hour, (d%time.Hour)/time.Minute)
	}
	return fmt.Sprintf("%02d:%02d", d/time.Minute, (d%time.Minute)/time.Second)
}

// chunkGate decides whether to buffer a rate-limit notice chunk (hold) or flush a previously buffered
// one (flush). A held notice is dropped by maybeRotate when the rate-limit error follows; it's flushed
// only when the turn produces REAL content or completes — NOT on a bookkeeping notification. That last
// point matters: the adapter emits a usage_update BETWEEN the notice chunk and the error, and flushing
// on it leaked the notice before maybeRotate could drop it (the reported "message wasn't suppressed").
// A single-rung preset (no failover to hide the notice behind) is skipped, so its limit message shows.
func (c *acpControl) chunkGate(line []byte) (hold bool, flush []byte) {
	// Holding applies only when a rotation could seamlessly resend (a credential session, or a
	// preset with a rotating ladder). The TERMINAL bookkeeping below runs for every selection kind
	// — an agent: (provider) session must still forget completed prompts, or the map leaks.
	gated := false
	if cred, preset := c.selection(); cred != "" {
		gated = true
	} else if preset != "" {
		if rot := c.presetRotation(); rot != nil && rot.rotates() {
			gated = true
		}
	}
	if s, text, ok := agentChunk(line); ok {
		if !gated {
			return false, nil
		}
		if hint := detectLimit(text, time.Now()); hint.limited && !hint.outputLimited {
			c.mu.Lock()
			c.heldChunk[s] = append(c.heldChunk[s], line...) // copies line's bytes; safe to keep
			c.mu.Unlock()
			return true, nil
		}
		// A real (non-limit) content chunk in the same turn: the notice was a genuine warning the turn
		// spoke around, so flush it just ahead of the content.
		return false, c.takeHeld(s)
	}
	// A prompt's TERMINAL response — result or error — flushes any held notice for its session. (A
	// rate-limit error is intercepted by maybeRotate before chunkGate, so an error here is a non-limit
	// failure; without flushing on it the notice would be orphaned and the tracking would leak.) Any
	// OTHER notification (usage_update, tool calls, …) leaves the buffer intact — see the doc above.
	if id := terminalResponseID(line); id != "" {
		c.mu.Lock()
		s := c.promptSession[id]
		delete(c.promptSession, id) // the prompt completed (or failed) — stop tracking it
		delete(c.waits, s)          // a finished turn breaks the consecutive-wait chain
		c.mu.Unlock()
		if s != "" {
			return false, c.takeHeld(s)
		}
	}
	return false, nil
}

// takeHeld returns and clears a session's buffered rate-limit notice (empty when none).
func (c *acpControl) takeHeld(s string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.heldChunk[s]
	delete(c.heldChunk, s)
	return held
}

// bumpWait counts one more consecutive all-limited wait for session and returns the total;
// clearWait breaks the chain (a free rotation, a completed turn, or the give-up itself).
func (c *acpControl) bumpWait(s string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits[s]++
	return c.waits[s]
}

func (c *acpControl) clearWait(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.waits, s)
}

// agentChunk reports whether line is an agent_message_chunk session/update, returning its session id
// and streamed text.
func agentChunk(line []byte) (session, text string, ok bool) {
	if !bytes.Contains(line, []byte("agent_message_chunk")) {
		return "", "", false
	}
	var m struct {
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/update" || m.Params.Update.SessionUpdate != "agent_message_chunk" {
		return "", "", false
	}
	return m.Params.SessionID, m.Params.Update.Content.Text, true
}

// captureTurn accumulates the assistant side of the carried history: message chunks and
// one-line tool narration build the in-progress turn's narrative (tail-bounded), and a
// prompt's terminal response flushes it as one history entry. Runs AFTER maybeRotate, so a
// rate-limited turn doesn't flush — its partial narrative stays buffered and completes when
// the transparent resend re-runs the turn.
func (c *acpControl) captureTurn(line []byte) {
	if s, text, ok := agentChunk(line); ok {
		c.appendTurn(s, text)
		return
	}
	if s, narration, ok := c.toolNarration(line); ok {
		if narration != "" {
			c.appendTurn(s, narration)
		}
		return
	}
	if id := terminalResponseID(line); id != "" {
		c.mu.Lock()
		if s := c.promptSession[id]; s != "" {
			if len(c.turnText[s]) > 0 {
				c.appendHistoryLocked(s, "assistant", string(c.turnText[s]))
			}
			delete(c.turnText, s)
			delete(c.toolTitle, s) // any tool call without a terminal update dies with its turn
		}
		c.mu.Unlock()
	}
}

// appendTurn adds text to a session's in-progress turn narrative, keeping the TAIL when it
// overflows the entry cap — conclusions land last.
func (c *acpControl) appendTurn(session, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	buf := append(c.turnText[session], text...)
	if len(buf) > historyEntryBytes {
		buf = buf[len(buf)-historyEntryBytes:]
	}
	c.turnText[session] = buf
}

// toolNarration turns tool_call/tool_call_update session updates into one-line narrative for
// the carried history — "[tool] title — status" on the call's TERMINAL update. Titles arrive on
// the initial tool_call and are remembered per id (a terminal update may not repeat them). Tool
// PAYLOADS are deliberately excluded: results dominate transcripts, go stale across a provider
// switch, and the narrative ("read X, edited Y, tests green") is what actually carries.
func (c *acpControl) toolNarration(line []byte) (session, narration string, ok bool) {
	if !bytes.Contains(line, []byte("toolCallId")) {
		return "", "", false
	}
	var m struct {
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				SessionUpdate string `json:"sessionUpdate"`
				ToolCallID    string `json:"toolCallId"`
				Title         string `json:"title"`
				Kind          string `json:"kind"`
				Status        string `json:"status"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/update" {
		return "", "", false
	}
	u := m.Params.Update
	if (u.SessionUpdate != "tool_call" && u.SessionUpdate != "tool_call_update") || u.ToolCallID == "" {
		return "", "", false
	}
	s := m.Params.SessionID
	c.mu.Lock()
	defer c.mu.Unlock()
	if u.Title != "" {
		if c.toolTitle[s] == nil {
			c.toolTitle[s] = map[string]string{}
		}
		c.toolTitle[s][u.ToolCallID] = u.Title
	}
	switch u.Status {
	case "completed", "failed", "cancelled":
	default:
		return s, "", true // remembered the title; nothing to narrate until the terminal update
	}
	title := c.toolTitle[s][u.ToolCallID]
	delete(c.toolTitle[s], u.ToolCallID)
	if title == "" {
		title = u.Kind
	}
	if title == "" {
		title = u.ToolCallID
	}
	const toolTitleBytes = 200
	if len(title) > toolTitleBytes {
		title = title[:toolTitleBytes] + "…"
	}
	return s, "\n[tool] " + title + " — " + u.Status + "\n", true
}

// appendHistoryLocked adds one carried-history entry (text already bounded by the caller for
// assistant turns; user prompts are head-bounded here), stamps the session's current lead as the
// history's origin, and evicts the oldest entries past the session budget — remembering that it
// did, so the preamble can say "earlier context omitted". Called with c.mu held.
func (c *acpControl) appendHistoryLocked(session, role, text string) {
	if len(text) > historyEntryBytes {
		text = text[:historyEntryBytes] // the HEAD — a long user prompt leads with the ask
	}
	h := c.history[session]
	if h == nil {
		h = &sessHistory{}
		c.history[session] = h
	}
	h.lead = c.lead
	h.entries = append(h.entries, histEntry{role: role, text: text})
	h.size += len(text)
	for h.size > c.carryBytes && len(h.entries) > 1 {
		h.size -= len(h.entries[0].text)
		h.entries = h.entries[1:]
		h.evicted = true
	}
}

// preambleLocked renders a session's carried history as the plain-text context block prepended
// to the first prompt after a re-create — labeled and honest about its fidelity. A trailing USER
// entry is dropped: it is the very message being (re)sent, so it must not appear twice. Returns
// "" when there's nothing worth carrying. Called with c.mu held.
func (c *acpControl) preambleLocked(session string) string {
	h := c.history[session]
	if h == nil {
		return ""
	}
	entries := h.entries
	if n := len(entries); n > 0 && entries[n-1].role == "user" {
		entries = entries[:n-1]
	}
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	origin := h.lead
	if origin == "" {
		origin = "the previous session"
	}
	fmt.Fprintf(&b, "[coop] This thread continues a conversation carried over from %s — the session was re-created (provider switch or lost transcript). The context below is best-effort: message text plus one-line tool narration; tool payloads/results are not included, so re-read anything you need to rely on. Continue the conversation naturally.\n\n--- conversation so far ---\n", origin)
	if h.evicted {
		b.WriteString("(…earlier context omitted — the carried history hit its budget…)\n\n")
	}
	for _, e := range entries {
		fmt.Fprintf(&b, "[%s] %s\n\n", e.role, e.text)
	}
	b.WriteString("--- end of carried context ---")
	return b.String()
}

// promptText extracts the concatenated text blocks of a session/prompt line ("" when none).
func promptText(line []byte) string {
	var m struct {
		Params struct {
			Prompt []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range m.Params.Prompt {
		if p.Type == "text" && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// wrapPromptLine prepends a text content block carrying the preamble to a session/prompt line,
// preserving everything else (the request id above all — the response must still answer the
// editor's request). Returns the original line untouched if it doesn't parse as expected.
func wrapPromptLine(line []byte, preamble string) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(line, &top) != nil || len(top["params"]) == 0 {
		return line
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(top["params"], &params) != nil {
		return line
	}
	var prompt []json.RawMessage
	if json.Unmarshal(params["prompt"], &prompt) != nil {
		return line
	}
	block, err := json.Marshal(map[string]string{"type": "text", "text": preamble})
	if err != nil {
		return line
	}
	newPrompt, err := json.Marshal(append([]json.RawMessage{block}, prompt...))
	if err != nil {
		return line
	}
	params["prompt"] = newPrompt
	rawParams, err := json.Marshal(params)
	if err != nil {
		return line
	}
	top["params"] = rawParams
	out, err := json.Marshal(top)
	if err != nil {
		return line
	}
	return append(out, '\n')
}

// terminalResponseID returns the request id of a TERMINAL response — one carrying a result
// or an error. A request/notification that merely mentions "error" in its params has no
// top-level error member, so it returns "".
func terminalResponseID(line []byte) string {
	if !bytes.Contains(line, []byte(`"result"`)) && !bytes.Contains(line, []byte(`"error"`)) {
		return ""
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil || len(m.ID) == 0 || (len(m.Result) == 0 && len(m.Error) == 0) {
		return ""
	}
	return string(m.ID)
}

// acpRateSignals returns the structured limit markers to match for a session led by
// lead: the adapter's own (each owns its wire format — see Agent.ACPRateLimitSignals),
// or, for a lead that isn't a registered agent (fusion fronts whichever agent leads the
// council), the union of every adapter's so no provider's limit goes unrecognized.
func acpRateSignals(lead string) []agents.ACPSignal {
	if a, ok := agents.Get(lead); ok {
		return a.ACPRateLimitSignals()
	}
	var all []agents.ACPSignal
	for _, n := range agents.Names() {
		if a, ok := agents.Get(n); ok {
			all = append(all, a.ACPRateLimitSignals()...)
		}
	}
	return all
}

// acpErrorLimitHint classifies a JSON-RPC error: prose detection (shared detectLimit)
// plus the adapter-owned structured signals. It carries no provider constants itself —
// a new agent brings its markers via ACPRateLimitSignals.
func acpErrorLimitHint(raw json.RawMessage, now time.Time, signals []agents.ACPSignal) limitHint {
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &msg)
	hint := detectLimit(msg.Message, now)

	var v any
	if json.Unmarshal(raw, &v) != nil {
		return hint
	}
	structuredRate, structuredOutput := false, false
	var proseReset time.Time
	walkJSONStrings(v, "", func(key, value string) {
		k := compactJSONName(key)
		vc := compactJSONName(value)
		for _, s := range signals {
			if (s.Key == "" || compactJSONName(s.Key) == k) && compactJSONName(s.Value) == vc {
				structuredRate = true
			}
		}
		// The output-token axis is deliberately SHARED, not per-agent: stopReason is the
		// ACP-protocol stop-reason field, finishReason the common upstream-API leak, and
		// length/MAX_TOKENS spell "output budget exhausted" across providers.
		if (k == "finishreason" || k == "stopreason") && (vc == "length" || vc == "maxtokens") {
			structuredOutput = true
		}
		// The reset time often hides in a NESTED string: codex-acp's top-level message is a
		// generic "Internal error" while the human notice — "You've hit your usage limit. …
		// try again at 4:28 PM." — rides in data.message. Mine every string for a stated
		// reset (earliest wins) so the wait targets it instead of the 5-minute default.
		if h := detectLimit(value, now); h.limited && !h.outputLimited && !h.resetAt.IsZero() {
			if proseReset.IsZero() || h.resetAt.Before(proseReset) {
				proseReset = h.resetAt
			}
		}
	})
	if structuredRate {
		hint.limited = true
		hint.outputLimited = false
	} else if structuredOutput {
		hint.limited = true
		hint.outputLimited = true
	}
	if hint.limited && !hint.outputLimited && hint.resetAt.IsZero() {
		hint.resetAt = proseReset
	}
	return hint
}

func walkJSONStrings(v any, key string, visit func(string, string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			walkJSONStrings(child, k, visit)
		}
	case []any:
		for _, child := range x {
			walkJSONStrings(child, key, visit)
		}
	case string:
		visit(key, x)
	}
}

func compactJSONName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
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
// raw JSON so any fields coop doesn't model survive. When the adapter emits its models in a `models`
// field instead of a native `model` configOption (gemini) and no `model` option is present, coop
// synthesizes one from that field so the editor renders a coop-owned model dropdown.
func (c *acpControl) rewriteConfigOptions(raw, models json.RawMessage, sid string) json.RawMessage {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return raw
	}
	// On a PRESET the preset's lead ladder owns the model — show the box's truth, never coop's
	// launch-time model over it.
	_, preset := c.selection()
	model := c.currentModel() // snapshot: a synthesized-model switch mutates it from the editor goroutine
	out := c.coopOptions()
	hasModel := false
	for _, item := range arr {
		var head struct {
			ID      string      `json:"id"`
			Options []acpOption `json:"options"`
		}
		_ = json.Unmarshal(item, &head)
		if stripConfigIDs[head.ID] || coopOwnedIDs[head.ID] {
			continue
		}
		if head.ID == "model" {
			hasModel = true
			c.cacheModels(parseClaudeModelOption(head.Options)) // free refresh of `coop models` for claude
			if preset == "" && model != "" && optionHasValue(head.Options, model) {
				item = withField(item, "currentValue", model) // default to coop's model; still switchable
			}
		}
		out = append(out, item)
	}
	// gemini-shape: no native model option, but a models field carrying the choices. Synthesize coop's
	// own `model` select and latch the lead so fromEditor/sessionReady route via session/set_model.
	if !hasModel {
		if synth := c.synthModelOption(models, preset, model); synth != nil {
			out = append(out, synth)
			c.cacheModels(parseGeminiModels(models)) // free refresh of `coop models` for gemini
			c.mu.Lock()
			c.leadUsesSetModel = true
			c.mu.Unlock()
		}
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

// cacheModels writes the lead's models to its per-agent cache during a normal ACP session —
// the free, opportunistic refresh that keeps `coop models` live for claude/gemini at zero
// extra cost (the session/new models are already parsed here). Best-effort: an empty list or
// a write error is ignored, so the plain `coop models` just falls back to the static list.
func (c *acpControl) cacheModels(models []modelInfo) {
	_ = writeModelsCache(c.cfg, c.lead, models)
}

// coopOptions builds coop's toolbar dropdowns in their fixed order: Provider (omitted for a
// fusion governor — see fusionLadderGuard), Account, Preset. Each shows the EFFECTIVE state of
// the one underlying selection, so after any switch the other two catch up on the next refresh.
func (c *acpControl) coopOptions() []json.RawMessage {
	c.mu.Lock()
	sel, lead, creds, fusion := c.sel, c.lead, c.creds, c.fusion
	c.mu.Unlock()
	var out []json.RawMessage
	if !fusion {
		others := c.spawnableProviders(lead)
		opts := make([]acpOption, 0, len(others)+1)
		opts = append(opts, acpOption{Value: lead, Name: displayName(lead), Description: "The current provider"})
		for _, p := range others {
			opts = append(opts, acpOption{Value: p, Name: displayName(p),
				Description: "Switch to " + displayName(p) + " — new thread, context carried best-effort"})
		}
		out = append(out, marshalSelect(coopProviderID, "Provider",
			"Who runs the session — switching re-creates the thread and carries the context best-effort", lead, opts))
	}
	acct := "auto"
	if v, ok := strings.CutPrefix(sel, "cred:"); ok && v != "" {
		acct = v
	}
	aopts := make([]acpOption, 0, len(creds)+1)
	aopts = append(aopts, acpOption{Value: "auto", Name: "Auto", Description: "The marked default account, or the preset's ladder"})
	for _, cr := range creds {
		aopts = append(aopts, acpOption{Value: cr, Name: cr, Description: "Switch to account " + cr + " — the conversation is preserved"})
	}
	out = append(out, marshalSelect(coopAccountID, "Account",
		"The lead's login — switching is transparent (shared session store)", acct, aopts))
	ps := "none"
	if v, ok := strings.CutPrefix(sel, "preset:"); ok && v != "" {
		ps = v
	}
	popts := make([]acpOption, 0, len(c.presets)+1)
	popts = append(popts, acpOption{Value: "none", Name: "None", Description: "No preset — the plain lead"})
	for _, p := range c.presets {
		popts = append(popts, acpOption{Value: p, Name: p, Description: "Run under preset " + p + " (its lead ladder + roles)"})
	}
	out = append(out, marshalSelect(coopPresetID, "Preset",
		"Orchestration recipe — its lead + ladder drive the session, its roles mount", ps, popts))
	return out
}

// displayName is a provider's human product name for the dropdowns ("Codex"), falling back to
// the grammar token for anything unregistered. Values stay tokens — only labels dress up.
func displayName(provider string) string {
	if a, ok := agents.Get(provider); ok {
		return a.DisplayName()
	}
	return provider
}

// marshalSelect renders one select configOption as raw JSON.
func marshalSelect(id, name, desc, current string, opts []acpOption) json.RawMessage {
	co := map[string]any{
		"id": id, "name": name, "description": desc,
		"category": "coop", "type": "select", "currentValue": current, "options": opts,
	}
	b, _ := json.Marshal(co)
	return b
}

// selectorSel maps a set of one of coop's selector dropdowns onto the ONE selection string.
// recognized=false → not a coop dropdown (the line passes through to the adapter). newSel==""
// → nothing to change (already current, an "auto"/"none" placeholder, or a value refused
// because it could never spawn — a bogus provider/account would send the respawn loop chasing
// a box that can never come up).
func (c *acpControl) selectorSel(configID, value string) (newSel string, recognized bool) {
	c.mu.Lock()
	lead, creds, fusion := c.lead, c.creds, c.fusion
	preset := strings.HasPrefix(c.sel, "preset:")
	c.mu.Unlock()
	switch configID {
	case coopProviderID:
		if value == lead || fusion || !agents.Valid(value) || len(accountsFor(c.cfg, value)) == 0 {
			return "", true
		}
		return "agent:" + value, true
	case coopAccountID:
		if value == "auto" || !slices.Contains(creds, value) {
			return "", true
		}
		return "cred:" + value, true
	case coopPresetID:
		if value == "none" {
			if preset { // leaving the preset: back to the lead's marked default account
				return "cred:" + c.cfg.DefaultProfileOf(lead), true
			}
			return "", true
		}
		if !slices.Contains(c.presets, value) {
			return "", true
		}
		return "preset:" + value, true
	case coopSetupID:
		// The retired mixed dropdown's value grammar, still accepted for editors whose persisted
		// default_config_options replay it at startup.
		if v, ok := strings.CutPrefix(value, "agent:"); ok {
			if !agents.Valid(v) || len(accountsFor(c.cfg, v)) == 0 || fusion {
				return "", true
			}
		}
		return value, true
	}
	return "", false
}

// spawnableProviders lists the OTHER registered providers with at least one signed-in account —
// the ones a provider switch could actually spawn. The current lead is excluded (its accounts
// are the cred: entries).
func (c *acpControl) spawnableProviders(lead string) []string {
	var out []string
	for _, p := range agents.Names() {
		if p != lead && len(accountsFor(c.cfg, p)) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// synthModelOption builds a coop-owned `model` select from an adapter's session/new `models` field
// (gemini: {availableModels:[{modelId,name,description?}], currentModelId}). Returns nil when the field
// is absent or carries no models. The current value shows coop's chosen model on a credential session
// (so the pick survives a box swap once sessionReady re-applies it); on a preset the box's own current
// model wins, since the preset ladder owns it.
func (c *acpControl) synthModelOption(models json.RawMessage, preset, model string) json.RawMessage {
	if len(models) == 0 {
		return nil
	}
	var m struct {
		CurrentModelID  string `json:"currentModelId"`
		AvailableModels []struct {
			ModelID     string `json:"modelId"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"availableModels"`
	}
	if json.Unmarshal(models, &m) != nil || len(m.AvailableModels) == 0 {
		return nil
	}
	opts := make([]acpOption, 0, len(m.AvailableModels))
	for _, am := range m.AvailableModels {
		opts = append(opts, acpOption{Value: am.ModelID, Name: am.Name, Description: am.Description})
	}
	current := m.CurrentModelID
	if preset == "" && model != "" && optionHasValue(opts, model) {
		current = model
	}
	co := map[string]any{
		"id": "model", "name": "Model", "description": "Model for the session",
		"category": "model", "type": "select", "currentValue": current, "options": opts,
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
	// Adapter-owned force-sets (Agent.ACPSessionConfig), sorted for a stable wire order.
	if a, ok := agents.Get(c.lead); ok {
		forced := a.ACPSessionConfig()
		for _, k := range slices.Sorted(maps.Keys(forced)) {
			msgs = append(msgs, c.setConfig(sid, k, forced[k]))
		}
	}
	// On a PRESET the preset's lead ladder owns the model — forcing coop's launch-time model here
	// would silently override it in the box.
	c.mu.Lock()
	model, setModel := c.model, c.leadUsesSetModel
	c.mu.Unlock()
	if _, preset := c.selection(); model != "" && preset == "" {
		// A lead that switches via session/set_model (gemini) has no `model` config option, so re-apply
		// the chosen model with its own method — this is what carries the pick across a box swap, since the
		// respawned box starts on its launch-time default. Others take the native set_config_option.
		if setModel {
			msgs = append(msgs, c.setModel(sid, model))
		} else {
			msgs = append(msgs, c.setConfig(sid, "model", model))
		}
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

// setModel builds a session/set_model request to the adapter (the ACP model-switch method gemini
// exposes: params {sessionId, modelId}), with an InjectPrefix id so the proxy swallows its response.
func (c *acpControl) setModel(sid, model string) []byte {
	c.mu.Lock()
	c.nextID++
	n := c.nextID
	c.mu.Unlock()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      acpproxy.InjectPrefix + itoa(n),
		"method":  "session/set_model",
		"params":  map[string]any{"sessionId": sid, "modelId": model},
	}
	b, _ := json.Marshal(req)
	return append(b, '\n')
}

// fromEditor intercepts the editor's set of coop's own selector: it updates the selection and asks
// the proxy to restart the box on the new credential/preset, replying to the editor itself (the
// adapter never sees coop_setup). It also translates a set of coop's SYNTHESIZED model dropdown
// (gemini) into the adapter's session/set_model. Native option sets (a real adapter model/effort/fast
// option) return handled=false so they pass through to the adapter unchanged.
func (c *acpControl) fromEditor(line []byte) (handled bool, resp []byte, toAdapter []byte, restart bool) {
	var h struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			ConfigID  string `json:"configId"`
			Value     string `json:"value"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &h) != nil {
		return false, nil, nil, false
	}
	// Remember each session's in-flight prompt so a rate-limit rotation/wait can re-send it, and
	// record its text in the carried history. If the session was just re-created (a provider
	// switch), this prompt is the first into the fresh thread — rewrite it with the history
	// preamble (the proxy forwards the rewrite through the normal path, editor id intact).
	if h.Method == "session/prompt" && h.Params.SessionID != "" && len(h.ID) > 0 {
		sid := h.Params.SessionID
		clone := append([]byte(nil), line...)
		var wrapped []byte
		c.mu.Lock()
		c.promptSession[string(h.ID)] = sid
		if text := promptText(line); text != "" {
			c.appendHistoryLocked(sid, "user", text)
		}
		if c.needPreamble[sid] {
			delete(c.needPreamble, sid)
			if pre := c.preambleLocked(sid); pre != "" {
				wrapped = wrapPromptLine(clone, pre)
				clone = wrapped // a resend of THIS turn must carry the context too
			}
		}
		c.lastPrompt[sid] = clone
		c.mu.Unlock()
		return false, nil, wrapped, false
	}
	// coop's synthesized model select (gemini): the adapter has no `model` config option, so translate
	// the set into its session/set_model and ack the editor ourselves. leadUsesSetModel proves this lead
	// is the synthesized-dropdown case; adapters with a native model option (claude, codex) fall through
	// and their set_config_option{model} passes straight to the adapter.
	if h.Method == "session/set_config_option" && h.Params.ConfigID == "model" {
		c.mu.Lock()
		synth := c.leadUsesSetModel
		c.mu.Unlock()
		if synth {
			return c.setModelFromEditor(h.ID, h.Params.SessionID, h.Params.Value)
		}
	}
	newSel, recognized := c.selectorSel(h.Params.ConfigID, h.Params.Value)
	if !recognized {
		return false, nil, nil, false
	}
	c.mu.Lock()
	// Only a REAL change restarts. Editors (Zed) apply default_config_options at startup by SETTING
	// a dropdown to the value it's already on; restarting on that no-op would respawn the box before
	// the session has any transcript, so the replayed session/load fails "Resource not found" and the
	// conversation is lost before it begins. A no-op (or a refused value) just re-acks.
	changed := newSel != "" && newSel != c.sel
	if changed {
		c.sel = newSel
	}
	cached := c.cached[h.Params.SessionID]
	c.mu.Unlock()
	// Ack with the full option set, coop_setup showing the CURRENT value. The cache was captured at
	// session/new with the old currentValue, so echoing it verbatim would revert the editor's dropdown;
	// rebuild coop_setup fresh.
	refreshed := c.refreshSetup(cached)
	if len(refreshed) > 0 && h.Params.SessionID != "" {
		c.mu.Lock()
		c.cached[h.Params.SessionID] = refreshed
		c.mu.Unlock()
	}
	result := map[string]json.RawMessage{}
	if len(refreshed) > 0 {
		result["configOptions"] = refreshed
	}
	out := map[string]any{"jsonrpc": "2.0", "id": h.ID, "result": result}
	b, _ := json.Marshal(out)
	return true, append(b, '\n'), nil, changed
}

// setModelFromEditor handles a set of coop's synthesized model dropdown: it records the pick as coop's
// model (so it rides the next box swap — but never over a preset, whose ladder owns the model), emits a
// session/set_model to the adapter for the live switch, and acks the editor with the refreshed option
// set (coop_setup + the model option showing the new value). No box restart — this is a live switch.
func (c *acpControl) setModelFromEditor(id json.RawMessage, sid, value string) (bool, []byte, []byte, bool) {
	_, preset := c.selection()
	c.mu.Lock()
	if value != "" && preset == "" {
		c.model = value
	}
	cached := c.cached[sid]
	c.mu.Unlock()
	refreshed := c.refreshModelAck(cached, value)
	if len(refreshed) > 0 && sid != "" {
		c.mu.Lock()
		c.cached[sid] = refreshed
		c.mu.Unlock()
	}
	result := map[string]json.RawMessage{}
	if len(refreshed) > 0 {
		result["configOptions"] = refreshed
	}
	ack := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	b, _ := json.Marshal(ack)
	var inject []byte
	if sid != "" && value != "" {
		inject = c.setModel(sid, value)
	}
	return true, append(b, '\n'), inject, false
}

// refreshModelAck rebuilds the cached option array with fresh coop dropdowns and the model
// option's currentValue set to value — the ack a synthesized-model set echoes so the editor's
// dropdown keeps the pick.
func (c *acpControl) refreshModelAck(cached json.RawMessage, value string) json.RawMessage {
	arr := c.refreshCoopOptions(cached)
	for i, it := range arr {
		var head struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(it, &head) == nil && head.ID == "model" {
			arr[i] = withField(it, "currentValue", value)
		}
	}
	b, err := json.Marshal(arr)
	if err != nil {
		return cached
	}
	return b
}

// refreshSetup returns the cached configOptions array with coop's dropdowns rebuilt fresh
// (currentValues = the effective selection). Falls back to just coop's dropdowns when there's
// no cache yet.
func (c *acpControl) refreshSetup(cached json.RawMessage) json.RawMessage {
	b, err := json.Marshal(c.refreshCoopOptions(cached))
	if err != nil {
		return cached
	}
	return b
}

// refreshCoopOptions strips every coop-owned dropdown from a cached configOptions array (the
// retired coop_setup included) and prepends freshly-built ones — the single place the "coop
// dropdowns lead, natives follow" order is enforced on a refresh.
func (c *acpControl) refreshCoopOptions(cached json.RawMessage) []json.RawMessage {
	var arr []json.RawMessage
	if len(cached) > 0 {
		_ = json.Unmarshal(cached, &arr)
	}
	out := c.coopOptions()
	for _, it := range arr {
		var head struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(it, &head) == nil && coopOwnedIDs[head.ID] {
			continue
		}
		out = append(out, it)
	}
	return out
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
