package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
// grammar: Preset (the orchestration recipe), Provider (who runs — switching re-creates the thread,
// context carried best-effort), and Account (the lead's login — a transparent switch).
// None is a native adapter option, so the proxy intercepts their session/set_config_option and
// restarts the box on the selected identity.
const (
	coopProviderID = "coop_provider"
	coopAccountID  = "coop_account"
	coopPresetID   = "coop_preset"
)

// coopOwnedIDs are the selector ids coop rebuilds on every refresh.
var coopOwnedIDs = map[string]bool{coopProviderID: true, coopAccountID: true, coopPresetID: true}

// stripConfigIDs are the native claude-agent-acp toolbar dropdowns coop removes: the permission-mode
// picker (coop always runs yolo — the box is the sandbox) and the subagent picker (subagents still
// auto-delegate; coop just drops the manual selector). model/effort/fast stay on a credential
// session; a preset hides them too (rewriteConfigOptions — the preset owns them).
var stripConfigIDs = map[string]bool{"mode": true, "agent": true}

// acpSelection is the tagged launch intent behind coop's toolbar. A plain selection may choose a
// provider and account; a preset selection owns both through its ladder, so Provider and Account are
// always empty while Preset is set. The fields are strings so the state is comparable and can key the
// preset rotation.
type acpSelection struct {
	Provider string `json:"provider,omitempty"`
	Account  string `json:"account,omitempty"`
	Preset   string `json:"preset,omitempty"`
}

func normalizeACPSelection(sel acpSelection) acpSelection {
	if sel.Preset != "" {
		sel.Provider = ""
		sel.Account = ""
	}
	return sel
}

// acpControl is coop's control layer over one ACP editor session. It rewrites the toolbar the editor
// sees (force yolo, default the model to coop's, drop mode/subagent, prepend coop's selectors) and
// handles their changes by restarting the box on the selected identity. The conversation survives
// because the transcript sits on a shared, credential-independent store.
type acpControl struct {
	cfg          *config.Config              // for expanding a selected preset's model ladder into rotation targets
	repo         string                      // repo root, to load a preset selected from the toolbar
	lead         string                      // the CURRENT lead agent — re-derived on a provider switch (see retarget)
	model        string                      // coop's resolved model for the lead ("" → leave the adapter's default)
	target       agents.Target               // complete active box/session intent, including preset model + effort
	plainTargets map[string]targetPreference // provider -> last accepted plain model/effort choice
	creds        []string                    // the lead's credentials (accounts), in order
	presets      []string                    // the repo's presets, in order
	fusion       bool                        // a fusion governor session — the selector offers no provider switch

	accounts []string // the lead's signed-in accounts, for rate-limit auto-rotation (default first)

	mu          sync.Mutex
	sel         acpSelection                 // tagged plain lead or preset-owned selection
	autoAccount string                       // concrete account behind Account=Auto after a recovery rotation
	authFailed  map[string]bool              // provider@account failures already tried in automatic mode
	cached      map[string]json.RawMessage   // sessionId -> the rewritten configOptions array (for set responses)
	nativeCache map[string]nativeOptionCache // sessionId -> provider-owned raw options/models truth
	// leadUsesSetModel latches true once a session/new result proves this lead exposes its models via a
	// `models` field with no native `model` configOption, so coop synthesized the dropdown. It
	// then routes an editor `model` set to session/set_model (fromEditor) and re-applies the chosen model
	// after a box swap (sessionReady). Stays false for adapters with a native model config option.
	leadUsesSetModel bool
	limited          map[string]time.Time // provider@account -> when its rate limit resets (skip until then)
	nextID           int
	nativePending    map[string]nativeTargetChange // request id -> target change awaiting adapter success
	nativeLatest     map[string]uint64             // provider+field -> newest request sequence
	nativeAccepted   map[string]uint64             // provider+field -> newest accepted request sequence
	nativeSequence   uint64
	recreate         map[string]bool // sessionId -> next replay must create fresh instead of loading

	// Preset rate-limit failover: a preset session rotates the lead's model ladder (fable→opus→…),
	// unlike a credential session (which rotates accounts on one model). rot is the active preset's
	// rotation (nil for a plain session); rotFor records the preset it was built from, so a preset
	// change rebuilds it.
	rot    *rotation
	rotFor acpSelection

	// Transparent rate-limit resend: correlate a rate-limit error (which carries only a request id)
	// back to its session, and re-send that prompt once the box is back on a fresh credential.
	promptSession map[string]string // in-flight session/prompt: request id -> editor sessionId
	lastPrompt    map[string][]byte // editor sessionId -> its latest prompt line (what to resend)
	resend        map[string]bool   // editor sessionId -> re-send the last prompt after the next restart
	heldChunk     map[string][]byte // editor sessionId -> buffered limit/auth notice awaiting the turn's outcome
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
	turnProvider map[string]string            // editor sessionId -> provider that produced the in-progress assistant turn
	turnActive   map[string]bool              // editor sessionId -> a prompt cleared proxy admission on the live child
	toolTitle    map[string]map[string]string // editor sessionId -> toolCallId -> title (until its terminal update)
	needPreamble map[string]bool              // editor sessionId -> wrap the next outgoing prompt with the history
	echoPreamble map[string]string            // editor sessionId -> exact injected carry block to remove from provider user echoes
}

// sessHistory is one session's carried conversation. Each entry owns its provider provenance because
// the selected provider can change before an in-flight response finishes. size tracks the entries'
// total bytes for the eviction cap; evicted notes that older context was dropped, so the preamble says
// so instead of implying completeness.
type sessHistory struct {
	entries []histEntry
	size    int
	evicted bool
}

type histEntry struct{ provider, role, text string }

type nativeTargetChange struct {
	field      string
	value      string
	provider   string
	session    string
	sequence   uint64
	translated bool
}

type nativeOptionCache struct {
	Provider string          `json:"provider"`
	Options  json.RawMessage `json:"options"`
	Models   json.RawMessage `json:"models,omitempty"`
}

type targetPreference struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// historyEntryBytes caps one entry's text. A user prompt keeps its HEAD (the ask leads); an
// assistant turn keeps its TAIL (conclusions land last). Plain bytes, not tokens — close
// enough for a best-effort carry. The per-session budget is cfg-owned (COOP_ACP_CARRY_TOKENS).
const historyEntryBytes = 16 << 10

func newACPControl(cfg *config.Config, lead, model, effort, repo string, sel acpSelection, presets, serveURLs []string, fusion bool) *acpControl {
	sel = normalizeACPSelection(sel)
	if effort == "" {
		effort = cfg.EffortFor(lead)
	}
	target := agents.Target{Provider: lead, Model: model, Effort: effort}
	return &acpControl{
		cfg: cfg, repo: repo, fusion: fusion,
		lead: lead, model: model, target: target, creds: cfg.Profiles(lead), presets: presets,
		plainTargets:   map[string]targetPreference{lead: {Model: model, Effort: effort}},
		accounts:       accountsFor(cfg, lead),
		sel:            sel,
		authFailed:     map[string]bool{},
		cached:         map[string]json.RawMessage{},
		nativeCache:    map[string]nativeOptionCache{},
		limited:        map[string]time.Time{},
		nativePending:  map[string]nativeTargetChange{},
		nativeLatest:   map[string]uint64{},
		nativeAccepted: map[string]uint64{},
		recreate:       map[string]bool{},
		promptSession:  map[string]string{},
		lastPrompt:     map[string][]byte{},
		resend:         map[string]bool{},
		heldChunk:      map[string][]byte{},
		waits:          map[string]int{},
		serveURLs:      serveURLs,
		reported:       map[string]bool{},
		carryBytes:     cfg.ACPCarryBytes(),
		history:        map[string]*sessHistory{},
		turnText:       map[string][]byte{},
		turnProvider:   map[string]string{},
		turnActive:     map[string]bool{},
		toolTitle:      map[string]map[string]string{},
		needPreamble:   map[string]bool{},
		echoPreamble:   map[string]string{},
	}
}

// hooks wires the controller into the acpproxy.
func (c *acpControl) hooks() *acpproxy.Hooks {
	return &acpproxy.Hooks{
		ToEditor:              c.toEditor,
		ReplayFailure:         c.maybeRecoverAuthentication,
		SessionReady:          c.sessionReady,
		InjectedResponse:      c.injectedResponse,
		ChildReset:            c.childReset,
		FromEditor:            c.fromEditor,
		PromptForwarded:       c.promptForwarded,
		AutoReply:             c.autoReply,
		ResumePrompt:          c.resumePrompt,
		ShouldRecreateSession: c.shouldRecreateSession,
		SessionRecreated:      c.sessionRecreated,
		SessionClosed:         c.sessionClosed,
		SessionEnded:          c.sessionEnded,
	}
}

func (c *acpControl) clearSessionActive(session string) {
	c.mu.Lock()
	delete(c.cached, session)
	delete(c.nativeCache, session)
	delete(c.recreate, session)
	delete(c.lastPrompt, session)
	delete(c.resend, session)
	delete(c.heldChunk, session)
	delete(c.waits, session)
	delete(c.reported, session)
	delete(c.turnText, session)
	delete(c.turnProvider, session)
	delete(c.turnActive, session)
	delete(c.toolTitle, session)
	delete(c.echoPreamble, session)
	for id, sid := range c.promptSession {
		if sid == session {
			delete(c.promptSession, id)
		}
	}
	c.mu.Unlock()
}

func (c *acpControl) sessionClosed(session string) {
	c.clearSessionActive(session)
}

func (c *acpControl) sessionEnded(session string) {
	c.clearSessionActive(session)
	c.mu.Lock()
	delete(c.history, session)
	delete(c.needPreamble, session)
	c.mu.Unlock()
}

// sessionRecreated marks a session whose conversation did not carry across a restart (the proxy
// re-created it fresh — a provider switch, or a lost transcript): the next outgoing prompt gets
// the best-effort history preamble, whether that's a transparent resend or the editor's next message.
func (c *acpControl) sessionRecreated(session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.recreate, session)
	c.needPreamble[session] = true
}

func (c *acpControl) shouldRecreateSession(session string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recreate[session]
}

// resumePrompt returns the prompt to transparently re-send for a session once its box is back after a
// rate-limit rotation/wait, or nil. This is deliberately a non-destructive peek: PromptForwarded
// consumes the flags only after the proxy writes the synthetic prompt to the still-current child.
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
	prompt := c.lastPrompt[session]
	if len(prompt) == 0 {
		return nil
	}
	// Preserve visible partial output before rendering context for a replacement provider. This
	// history mutation is intentionally durable across a superseded candidate; only the resend and
	// preamble flags wait for confirmed prompt admission.
	c.closeProviderTurnLocked(session, c.lead)
	// The restart that triggered this resend may have re-created the session (a cross-provider
	// rotation): the resent prompt is then the first into a fresh thread, so it carries the
	// history preamble. Do not consume the flag until PromptForwarded confirms admission.
	if c.needPreamble[session] && len(prompt) > 0 {
		if pre := c.preambleLocked(session, promptText(prompt) != ""); pre != "" {
			prompt = wrapPromptLine(prompt, pre)
		}
	}
	return prompt
}

// selection returns one consistent snapshot for resolver and toolbar reads.
func (c *acpControl) selection() acpSelection {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sel = normalizeACPSelection(c.sel)
	return c.sel
}

// currentModel is coop's chosen model for the lead. Read under the lock because a set of Coop's
// synthesized models-field dropdown mutates it from the editor goroutine while the box→editor
// goroutine reads it to render the toolbar.
func (c *acpControl) currentModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.model
}

// presetRotation returns the active preset's model-ladder rotation, built once per preset from the
// preset's lead ladder (expandLadder — the SAME targets `coop loop` cycles). Returns nil for a plain
// session or when the ladder can't be expanded. The preset owns the full target, including any
// cross-provider and account fan-out. Its cursor and per-target limits persist across respawns.
func (c *acpControl) presetRotation() *rotation {
	sel := c.selection()
	if sel.Preset == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rot != nil && c.rotFor == sel {
		return c.rot
	}
	c.rotFor, c.rot = sel, nil
	p, err := preset.Load(c.repo, c.cfg.GlobalPresetsDir(), sel.Preset)
	if err != nil {
		return nil
	}
	ladder := slices.Clone(p.LeadLadder)
	// The whole ladder, cross-provider rungs included: the respawn env carries a full target,
	// so a rung on another provider swaps the lead (the proxy re-creates the session there and
	// the conversation is carried best-effort as a text preamble). Account fan-out is part of
	// expandLadder, so the preset remains authoritative for every rung.
	targets, err := expandLadder(c.cfg, p.LeadAgent, ladder)
	if err != nil || len(targets) == 0 {
		return nil
	}
	c.rot = newRotation(targets)
	return c.rot
}

// spawnTarget resolves what the NEXT box spawn runs — the full target (provider, model,
// account) the factory exports as COOP_ACP_TARGET, plus the preset whose roles mount — and
// re-derives the control's per-lead state when the provider changes hands (a manual plain-provider
// switch or a cross-provider preset rung). ok=false only when the
// selection is unrecognizable (the factory then spawns on the launch args alone).
func (c *acpControl) spawnTarget() (t agents.Target, presetName string, ok bool) {
	sel := c.selection()
	var rung *agents.Target
	if sel.Preset != "" {
		if rot := c.presetRotation(); rot != nil { // locks internally — resolve before taking c.mu
			r := rot.active()
			rung = &r
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case sel.Preset != "":
		if rung != nil {
			t = *rung // the active ladder rung — provider included (a cross-provider rung swaps the lead)
		} else {
			// No rotation means either no signed-in rungs or an unloadable preset. The inner
			// still mounts the preset's roles and applies this target last.
			provider := c.presetLead(sel.Preset, c.lead)
			t = agents.Target{Provider: provider}
		}
	default:
		provider := sel.Provider
		if provider == "" {
			provider = c.lead
		}
		if provider == "" {
			return agents.Target{}, "", false
		}
		// Retarget before resolving Auto: autoAccount and accounts belong to the provider that will
		// actually spawn. Resolving first can leak claude@work into a Codex switch.
		c.retargetLocked(provider)
		// Each provider retains its own last accepted plain target. Switching providers changes the
		// capability namespace without discarding the choice to restore when the user switches back.
		// Read it even when provider == c.lead: leaving a preset on its effective provider is a
		// same-provider transition, but its ladder target must not leak into the plain selection.
		pref, ok := c.plainTargets[provider]
		if !ok {
			pref = targetPreference{Model: c.cfg.ModelFor(provider), Effort: c.cfg.EffortFor(provider)}
			c.plainTargets[provider] = pref
		}
		c.model = pref.Model
		model, effort := pref.Model, pref.Effort
		t = agents.Target{Provider: provider, Model: model, Effort: effort}
		account := sel.Account
		if account == "" {
			account = c.autoAccount
			if account == "" && len(c.accounts) > 0 {
				account = c.accounts[0]
				c.autoAccount = account
			}
		}
		if account != "" {
			t.Accounts = []string{account}
		}
	}
	c.retargetLocked(t.Provider)
	if t.Model == "" {
		t.Model = c.cfg.ModelFor(t.Provider)
	}
	if t.Effort == "" {
		t.Effort = c.cfg.EffortFor(t.Provider)
	}
	c.target = t
	return t, sel.Preset, true
}

// retargetLocked re-derives the per-lead state when the next spawn's provider differs from the
// current lead: the selector's credential list, the auto-rotation accounts, and the set-model
// latch all belong to the NEW lead. Provider-scoped target choices and cooldowns stay available so
// switching away and back restores the user's last accepted model/effort without retrying a cooling
// login. Called with c.mu held.
func (c *acpControl) retargetLocked(provider string) {
	if provider == "" || provider == c.lead {
		return
	}
	c.lead = provider
	c.creds = c.cfg.Profiles(provider)
	c.accounts = accountsFor(c.cfg, provider)
	c.autoAccount = ""
	if c.plainTargets == nil {
		c.plainTargets = map[string]targetPreference{}
	}
	pref, ok := c.plainTargets[provider]
	if !ok {
		pref = targetPreference{Model: c.cfg.ModelFor(provider), Effort: c.cfg.EffortFor(provider)}
		c.plainTargets[provider] = pref
	}
	c.model = pref.Model
	c.target = agents.Target{Provider: provider, Model: pref.Model, Effort: pref.Effort}
	c.leadUsesSetModel = false
}

// waitForReset blocks until a rate-limited credential's reset passes (or ctx is done), so a respawn the
// wait-for-reset path pointed at a still-cooling account only starts once it's usable. A no-op for an
// account that isn't limited — the common case, including a normal rotation to a free account.
func (c *acpControl) waitForReset(ctx context.Context, provider, cred string) {
	c.mu.Lock()
	until := c.limited[accountLimitKey(provider, cred)]
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
// watches for authentication/rate-limit errors and auto-rotates when possible (restart=true).
func (c *acpControl) toEditor(line []byte) (out []byte, restart bool) {
	targetResult := c.commitNativeTarget(line)
	if targetResult.tracked && targetResult.change.translated {
		return c.translatedModelResponse(line, targetResult), targetResult.restart
	}
	if filtered, matched := c.filterPreambleEcho(line); matched {
		if len(filtered) == 0 {
			return nil, false
		}
		line = filtered
	}
	// Authentication failure is target health, not an adapter-owned editor flow: Coop credentials are
	// mounted before launch. Rotate a plain automatic account, or keep a preset/pin exact and replace
	// the generic error with its login command. This also drops a duplicate auth-required chunk.
	if rewritten, restart, handled := c.maybeRecoverAuthentication(line); handled {
		return rewritten, restart
	}
	// A rate-limit error → rotate to a free account (or wait for the nearest reset) and re-send the
	// prompt transparently; maybeRotate also drops any buffered rate-limit notice for that turn.
	if rewritten, rotated := c.maybeRotate(line); rotated {
		return rewritten, true
	}
	line = hideAuthenticationMethods(line)
	// Buffer a rate-limit notice chunk until the turn's outcome is known — suppressed if the turn then
	// rate-limits (a seamless resend), flushed otherwise. This never drops a legit chunk that merely
	// mentions "rate limit"/"quota"/429, because a chunk is only dropped when a rate-limit error follows.
	if hold, flush := c.chunkGate(line); hold {
		return nil, false
	} else if flush != nil {
		c.captureTurnFrames(flush)
		c.captureTurn(line)
		out = append(flush, c.rewriteToEditor(line)...)
	} else {
		c.captureTurn(line)
		out = c.rewriteToEditor(line)
	}
	// Announce this repo's published ports once, when a session is established (its session/new result).
	if notice := c.serveNoticeFor(line); notice != nil {
		out = append(out, notice...)
	}
	return out, false
}

// maybeRecoverAuthentication turns a terminal auth_required error into Coop's credential policy.
// An automatic plain account advances once through the signed-in accounts. Presets and pinned plain
// accounts stay on their exact target: preset ladders are rate-limit fallbacks, so auth must never
// silently change the answering provider. A correlated Auto prompt is resent after restart; every
// non-rotating path names the exact `coop login provider@account` recovery command.
func (c *acpControl) maybeRecoverAuthentication(line []byte) (out []byte, restart, handled bool) {
	if !bytes.Contains(line, []byte(`"error"`)) || !authenticationError(line) {
		return nil, false, false
	}
	var response struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(line, &response) != nil || len(response.ID) == 0 {
		return nil, false, false
	}

	sel := c.selection()
	c.mu.Lock()
	provider := c.lead
	accounts := slices.Clone(c.accounts)
	session := c.promptSession[string(response.ID)]
	delete(c.promptSession, string(response.ID))
	canResend := session != "" && len(c.lastPrompt[session]) > 0
	if session != "" {
		delete(c.heldChunk, session)
		delete(c.turnActive, session)
	}
	currentAccount := sel.Account
	if sel.Preset != "" {
		currentAccount = c.target.Account()
	}
	if currentAccount == "" {
		currentAccount = c.autoAccount
	}
	if currentAccount == "" && len(accounts) > 0 {
		currentAccount = accounts[0]
	}
	c.mu.Unlock()
	if currentAccount == "" {
		currentAccount = c.cfg.ActiveProfile(provider)
	}

	if sel.Preset != "" {
		return rewriteAuthenticationRecovery(line, provider, currentAccount), false, true
	}

	// Account == "" is the explicit Auto policy. Track the concrete replacement separately so the UI
	// remains Auto and each signed-in account is tried at most once instead of silently becoming pinned.
	if sel.Account == "" {
		now := time.Now()
		c.mu.Lock()
		if c.authFailed == nil {
			c.authFailed = map[string]bool{}
		}
		c.authFailed[loginTarget(provider, currentAccount)] = true
		var next, earliest string
		var earliestReset time.Time
		for _, account := range accounts {
			if account == currentAccount || c.authFailed[loginTarget(provider, account)] {
				continue
			}
			until := c.limited[accountLimitKey(provider, account)]
			if !until.After(now) {
				next = account
				break
			}
			if earliest == "" || until.Before(earliestReset) {
				earliest, earliestReset = account, until
			}
		}
		if next == "" {
			next = earliest // all remaining accounts are cooling; the factory waits for the soonest
		}
		if next != "" {
			c.autoAccount = next
			if canResend {
				c.resend[session] = true
			}
		}
		c.mu.Unlock()
		if next != "" {
			if canResend {
				acpproxy.Trace("authentication failed on %s@%s: rotating to %s + auto-resending", provider, currentAccount, next)
				return c.configOptionUpdate(session), true, true
			}
			msg := fmt.Sprintf("coop: authentication failed for %s@%s; switched to %s@%s. Retry the request. To repair the failed account, run: coop login %s",
				provider, currentAccount, provider, next, loginTarget(provider, currentAccount))
			return rewriteErrorMessage(line, msg), true, true
		}
	}
	return rewriteAuthenticationRecovery(line, provider, currentAccount), false, true
}

func loginTarget(provider, account string) string {
	if account == "" {
		account = config.DefaultProfile
	}
	return provider + "@" + account
}

func rewriteAuthenticationRecovery(line []byte, provider, account string) []byte {
	return rewriteErrorMessage(line, fmt.Sprintf(
		"coop: authentication failed for %s. Re-authenticate it with: coop login %s",
		loginTarget(provider, account), loginTarget(provider, account),
	))
}

// hideAuthenticationMethods removes provider-owned login actions from Coop's immutable editor-facing
// initialize contract. Provider switches negotiate fresh capabilities internally, but ACP has no
// notification that can replace the editor's original auth menu; leaving it visible produces a stale
// generic Authenticate button for the wrong provider. Coop's Account selector and login command own it.
func hideAuthenticationMethods(line []byte) []byte {
	if !bytes.Contains(line, []byte(`"authMethods"`)) && !bytes.Contains(line, []byte(`"auth"`)) {
		return line
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(line, &message) != nil {
		return line
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(message["result"], &result) != nil || len(result["protocolVersion"]) == 0 {
		return line
	}
	result["authMethods"] = json.RawMessage("[]")
	if raw := result["agentCapabilities"]; len(raw) > 0 {
		var capabilities map[string]json.RawMessage
		if json.Unmarshal(raw, &capabilities) == nil {
			delete(capabilities, "auth") // newer ACP versions advertise logout here
			if encodedCapabilities, err := json.Marshal(capabilities); err == nil {
				result["agentCapabilities"] = encodedCapabilities
			}
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return line
	}
	message["result"] = encoded
	encoded, err = json.Marshal(message)
	if err != nil {
		return line
	}
	return append(encoded, '\n')
}

type nativeTargetResult struct {
	change   nativeTargetChange
	tracked  bool
	latest   bool
	success  bool
	accepted string
	restart  bool
}

func nativeTargetKey(provider, field string) string { return provider + "\x00" + field }

// armPromptResendsLocked keeps already-admitted turns alive across an intentional child replacement.
// Called with c.mu held.
func (c *acpControl) armPromptResendsLocked(reason string) {
	for id, sid := range c.promptSession {
		if c.lastPrompt[sid] == nil {
			continue
		}
		c.resend[sid] = true
		delete(c.turnActive, sid)
		delete(c.heldChunk, sid)
		delete(c.waits, sid)
		acpproxy.Trace("%s: re-sending the in-flight prompt %s for session %s after the swap", reason, id, sid)
	}
}

func (c *acpControl) registerNativeTarget(id, field, value, session string, translated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nativeSequence++
	change := nativeTargetChange{
		field: field, value: value, provider: c.lead, session: session,
		sequence: c.nativeSequence, translated: translated,
	}
	c.nativePending[id] = change
	c.nativeLatest[nativeTargetKey(change.provider, change.field)] = change.sequence
}

func (c *acpControl) commitNativeTarget(line []byte) nativeTargetResult {
	var response struct {
		ID    json.RawMessage `json:"id"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &response) != nil || len(response.ID) == 0 {
		return nativeTargetResult{}
	}
	c.mu.Lock()
	change, ok := c.nativePending[string(response.ID)]
	delete(c.nativePending, string(response.ID))
	result := nativeTargetResult{change: change, tracked: ok}
	if ok {
		key := nativeTargetKey(change.provider, change.field)
		result.latest = c.nativeLatest[key] == change.sequence
		if result.latest {
			delete(c.nativeLatest, key)
		}
		hasError := len(response.Error) > 0 && string(response.Error) != "null"
		activeProvider := normalizeACPSelection(c.sel).Preset == "" && c.lead == change.provider && c.target.Provider == change.provider
		newerPending := c.hasNewerNativeTargetLocked(key, change.sequence)
		sequenceCanWin := result.latest || (!newerPending && change.sequence > c.nativeAccepted[key])
		restartRequired := sequenceCanWin && activeProvider && change.translated && hasError && modelRestartSuggested(response.Error)
		result.success = !hasError || restartRequired
		if c.plainTargets == nil {
			c.plainTargets = map[string]targetPreference{}
		}
		pref := c.plainTargets[change.provider]
		if result.success && change.sequence > c.nativeAccepted[key] {
			c.nativeAccepted[key] = change.sequence
			switch change.field {
			case "model":
				pref.Model = change.value
			case "effort":
				pref.Effort = change.value
			}
			c.plainTargets[change.provider] = pref
		}
		effective := result.latest || (result.success && !newerPending && c.nativeAccepted[key] == change.sequence)
		if effective && activeProvider {
			switch change.field {
			case "model":
				c.model = pref.Model
				c.target.Model = pref.Model
			case "effort":
				c.target.Effort = pref.Effort
			}
		}
		result.restart = restartRequired
		if result.restart {
			if c.recreate == nil {
				c.recreate = map[string]bool{}
			}
			// The selected launch model owns the replacement child, so every active session on
			// that child must move to a fresh native session, not only the toolbar that changed it.
			for session := range c.nativeCache {
				c.recreate[session] = true
			}
			c.recreate[change.session] = true
			c.armPromptResendsLocked("model migration to " + change.value)
		}
		if change.field == "model" {
			result.accepted = pref.Model
		} else {
			result.accepted = pref.Effort
		}
	}
	c.mu.Unlock()
	return result
}

// hasNewerNativeTargetLocked reports whether an unresolved request for key must still get the last
// word. Responses can arrive in either order, so the latest request may reject before an older valid
// choice succeeds. Called with c.mu held after the current response was removed from nativePending.
func (c *acpControl) hasNewerNativeTargetLocked(key string, sequence uint64) bool {
	for _, pending := range c.nativePending {
		if pending.sequence > sequence && nativeTargetKey(pending.provider, pending.field) == key {
			return true
		}
	}
	return false
}

func (c *acpControl) childReset() {
	c.mu.Lock()
	c.nativePending = map[string]nativeTargetChange{}
	c.nativeLatest = map[string]uint64{}
	for session, provider := range c.turnProvider {
		if c.resend[session] {
			continue // an intentional retry owns this partial and resumes it on the replacement child
		}
		if len(c.turnText[session]) > 0 {
			c.appendHistoryLocked(session, provider, "assistant", string(c.turnText[session]))
		}
		delete(c.turnText, session)
		delete(c.turnProvider, session)
		delete(c.toolTitle, session)
		delete(c.heldChunk, session)
		delete(c.waits, session)
	}
	c.promptSession = map[string]string{}
	c.turnActive = map[string]bool{}
	c.echoPreamble = map[string]string{}
	c.mu.Unlock()
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
// carries NO configOptions (an adapter may expose only `models`, or neither) it still injects Coop's
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
		// a box swap. Rewrite wherever they sit, so coop's selectors plus stripped mode/agent and model
		// retargeting survive both; missing the nested shape dropped the coop toolbar after a switch.
		sid := sessionIDOf(inner)
		changed := false
		if _, hasModes := inner["modes"]; hasModes {
			delete(inner, "modes") // coop is always yolo; no permission-mode dropdown
			changed = true
		}
		if _, hasCO := inner["configOptions"]; hasCO {
			inner["configOptions"] = c.rewriteConfigOptions(inner["configOptions"], inner["models"], sid, false)
			changed = true
		} else if key == "result" && sid != "" {
			// A session/new|load|resume result with no configOptions. Coop still owns the toolbar, so
			// synthesize one from an empty set
			// (rewriteConfigOptions prepends coop's selectors, plus a model select when the result carries a
			// `models` field); without this the credential/preset switcher never appears for those agents
			// (the original missing-toolbar failure).
			inner["configOptions"] = c.rewriteConfigOptions(json.RawMessage("[]"), inner["models"], sid, false)
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
// config_option_update bypasses coop's toolbar rewrite and its selectors vanish after a switch.
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
	var replay bool
	_ = json.Unmarshal(u["coopReplay"], &replay)
	models := u["models"]
	delete(u, "coopReplay") // proxy-private replay metadata must not cross the editor boundary
	delete(u, "models")
	u["configOptions"] = c.rewriteConfigOptions(u["configOptions"], models, sid, replay)
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
	sel := c.selection()
	var h struct {
		ID    json.RawMessage `json:"id"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &h) != nil || len(h.Error) == 0 {
		return nil, false
	}
	now := time.Now()
	c.mu.Lock()
	provider := c.lead
	currentAccount := sel.Account
	if currentAccount == "" {
		currentAccount = c.autoAccount
		if currentAccount == "" && len(c.accounts) > 0 {
			currentAccount = c.accounts[0]
		}
	}
	c.mu.Unlock()
	hint := acpErrorLimitHint(h.Error, now, acpRateSignals(provider))
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
		delete(c.turnActive, session)
	}
	c.mu.Unlock()

	// A preset session rotates its model ladder (a rung is a model+account); a credential session
	// rotates accounts on the fixed model (below).
	if sel.Preset != "" {
		return c.rotatePreset(session, canResend, until, now)
	}

	next := c.nextAccount(provider, currentAccount, until, now)
	if next != "" {
		c.clearWait(session) // a free rotation breaks the consecutive-wait chain
		c.mu.Lock()
		if sel.Account == "" {
			c.autoAccount = next
		} else {
			c.sel.Account = next
		}
		if canResend {
			c.resend[session] = true
		}
		c.mu.Unlock()
		if canResend {
			acpproxy.Trace("rate limit on %s@%s: rotating to %s + auto-resending", provider, currentAccount, next)
			// Swallow the error, move the toolbar dropdown to the new credential, restart on it, and
			// re-send after replay — the config_option_update is the only thing the editor sees.
			return c.configOptionUpdate(session), true
		}
		// Couldn't identify the prompt — fall back to switching + asking the user to resend.
		return rewriteErrorMessage(line, fmt.Sprintf("coop: %s is rate limited — switched to %s; resend your last message", currentAccount, next)), true
	}

	// Nothing free right now: wait for the nearest reset, then re-send on that account. Needs a known
	// reset and a captured prompt; otherwise forward the error so the user can wait/retry themselves.
	acct, at := c.nearestReset(provider)
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
	if sel.Account == "" {
		c.autoAccount = acct
	} else {
		c.sel.Account = acct
	}
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
// because the factory reads the active rotation target; the toolbar catches up through the replay's
// config_option_update, while the Preset selector stays unchanged.
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
// configOptions) with coop's current selector state rebuilt — so the editor's toolbar reflects an
// auto-switch coop made (a rate-limit rotation/wait), just as a manual switch's ack does. Falls back
// to just coop's selectors if this session's options weren't cached.
func (c *acpControl) configOptionUpdate(session string) []byte {
	upd := map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": session,
			"update": map[string]any{
				"sessionUpdate": "config_option_update",
				"configOptions": json.RawMessage(c.refreshSetup(session)),
			},
		},
	}
	b, _ := json.Marshal(upd)
	return append(b, '\n')
}

// nearestReset returns the signed-in account whose rate limit resets soonest (and when), or "" if none
// are marked limited. Used when no account is free right now: coop waits for this one, then re-sends.
func (c *acpControl) nearestReset(provider string) (account string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.accounts {
		t, ok := c.limited[accountLimitKey(provider, a)]
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

// chunkGate buffers a limit/auth notice chunk until the turn reveals whether it is duplicate error UI
// or real content. A limit notice is dropped by maybeRotate when transparent resend follows; an auth
// notice is dropped when its terminal auth error follows. Otherwise the notice is flushed when the turn
// produces real content or completes — never on an intermediate bookkeeping notification. A single-rung
// preset (no failover to hide a limit notice behind) still shows its limit message immediately.
func (c *acpControl) chunkGate(line []byte) (hold bool, flush []byte) {
	// Holding applies only when a rotation could seamlessly resend (a credential session, or a
	// preset with a rotating ladder). The TERMINAL bookkeeping below runs for every selection kind
	// — a provider-only session must still forget completed prompts, or the map leaks.
	gated := false
	sel := c.selection()
	c.mu.Lock()
	hasAccounts := len(c.accounts) > 0
	c.mu.Unlock()
	if sel.Preset == "" && (sel.Account != "" || hasAccounts) {
		gated = true
	} else if sel.Preset != "" {
		if rot := c.presetRotation(); rot != nil && rot.rotates() {
			gated = true
		}
	}
	if s, text, ok := agentChunk(line); ok {
		if authenticationRequired(text) {
			c.mu.Lock()
			active := c.turnActive[s]
			if active {
				c.heldChunk[s] = append(c.heldChunk[s], line...)
			}
			c.mu.Unlock()
			if active {
				return true, nil
			}
		}
		if !gated {
			return false, nil
		}
		if hint := detectLimit(text, time.Now()); hint.limited && !hint.outputLimited {
			c.mu.Lock()
			active := c.turnActive[s]
			if active {
				c.heldChunk[s] = append(c.heldChunk[s], line...) // copies line's bytes; safe to keep
			}
			c.mu.Unlock()
			if active {
				return true, nil
			}
			return false, nil
		}
		// A real (non-limit) content chunk in the same turn: the notice was a genuine warning the turn
		// spoke around, so flush it just ahead of the content.
		return false, c.takeHeld(s)
	}
	// A prompt's TERMINAL response — result or error — releases any held notice for its session. (A
	// rate-limit error is intercepted by maybeRotate before chunkGate, so an error here is a non-limit
	// failure; without releasing on it the notice would be orphaned and the tracking would leak.) The
	// mapping is cleared by captureTurn after the released chunks are captured. Any OTHER notification
	// (usage_update, tool calls, …) leaves the buffer intact — see the doc above.
	if id := terminalResponseID(line); id != "" {
		c.mu.Lock()
		s := c.promptSession[id]
		c.mu.Unlock()
		if s != "" {
			if authenticationError(line) {
				c.takeHeld(s) // the terminal error is the editor-visible verdict; drop its duplicate chunk
				return false, nil
			}
			return false, c.takeHeld(s)
		}
	}
	return false, nil
}

func authenticationRequired(text string) bool {
	compact := compactJSONName(text)
	return compact == "authrequired" ||
		strings.Contains(compact, "authenticationrequired") ||
		strings.Contains(compact, "notauthenticated") ||
		strings.Contains(compact, "signinrequired")
}

func authenticationError(line []byte) bool {
	var h struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(line, &h) != nil || h.Error == nil {
		return false
	}
	foundExact, foundMessage := false, false
	walkJSONStrings(h.Error, "", func(key, value string) {
		compactKey, compactValue := compactJSONName(key), compactJSONName(value)
		switch compactKey {
		case "reason", "errorkind", "type", "code":
			foundExact = foundExact || compactValue == "authrequired" || compactValue == "authenticationrequired" || compactValue == "notauthenticated"
		case "message":
			foundMessage = foundMessage || authenticationRequired(value)
		}
	})
	return foundExact || foundMessage
}

// takeHeld returns and clears a session's buffered limit/auth notice (empty when none).
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
// prompt's terminal response flushes it as one history entry. Runs AFTER maybeRotate and chunkGate,
// so a suppressed rate-limit notice is never captured; a held warning that proved legitimate is
// captured only when chunkGate releases it. A rate-limited turn's real partial narrative stays
// buffered and completes when the transparent resend re-runs the turn.
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
				c.appendHistoryLocked(s, c.turnProvider[s], "assistant", string(c.turnText[s]))
			}
			delete(c.turnText, s)
			delete(c.turnProvider, s)
			delete(c.turnActive, s)
			delete(c.toolTitle, s) // any tool call without a terminal update dies with its turn
			delete(c.echoPreamble, s)
			delete(c.waits, s) // a finished turn breaks the consecutive-wait chain
		}
		delete(c.promptSession, id)
		c.mu.Unlock()
	}
}

// captureTurnFrames captures newline-delimited frames released by chunkGate. ACP uses one JSON object
// per line; blank lines are ignored. Keeping this parser here ensures multiple held warning chunks are
// all represented if the turn continues normally.
func (c *acpControl) captureTurnFrames(lines []byte) {
	for _, line := range bytes.Split(lines, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 {
			c.captureTurn(line)
		}
	}
}

// closeProviderTurnLocked preserves already-visible partial output when a replacement provider takes
// over an in-flight turn. Called with c.mu held.
func (c *acpControl) closeProviderTurnLocked(session, provider string) {
	previous := c.turnProvider[session]
	if previous != "" && previous != provider {
		if len(c.turnText[session]) > 0 {
			c.appendHistoryLocked(session, previous, "assistant", string(c.turnText[session]))
		}
		delete(c.turnText, session)
		delete(c.toolTitle, session)
	}
}

// beginTurnLocked attributes subsequent assistant output to an admitted prompt's provider.
func (c *acpControl) beginTurnLocked(session, provider string) {
	c.closeProviderTurnLocked(session, provider)
	c.turnProvider[session] = provider
}

// promptForwarded runs at the proxy's post-gate admission boundary for both real editor prompts and
// synthetic resends. FromEditor owns raw user/history mutation; this hook owns only in-flight request
// correlation and assistant provenance, so a rejected target-setting chain cannot create phantom state.
func (c *acpControl) promptForwarded(line []byte, synthetic bool) {
	var h struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &h) != nil || h.Method != "session/prompt" || len(h.ID) == 0 || h.Params.SessionID == "" {
		return
	}
	c.mu.Lock()
	if synthetic {
		delete(c.resend, h.Params.SessionID)
		delete(c.needPreamble, h.Params.SessionID)
	}
	c.promptSession[string(h.ID)] = h.Params.SessionID
	if preamble := carriedPreamble(line); preamble != "" {
		c.echoPreamble[h.Params.SessionID] = preamble
	}
	c.beginTurnLocked(h.Params.SessionID, c.lead)
	c.turnActive[h.Params.SessionID] = true
	c.mu.Unlock()
}

// filterPreambleEcho removes only the exact carry block Coop injected into an admitted prompt. Some
// adapters echo prompt content as user_message_chunk; forwarding that block makes the editor render a
// giant synthetic user bubble. Any real user text before or after the exact block is preserved.
func (c *acpControl) filterPreambleEcho(line []byte) ([]byte, bool) {
	if !bytes.Contains(line, []byte("user_message_chunk")) {
		return line, false
	}
	var message map[string]json.RawMessage
	var params map[string]json.RawMessage
	var update map[string]json.RawMessage
	var content map[string]json.RawMessage
	if json.Unmarshal(line, &message) != nil || json.Unmarshal(message["params"], &params) != nil ||
		json.Unmarshal(params["update"], &update) != nil || json.Unmarshal(update["content"], &content) != nil {
		return line, false
	}
	var method, session, kind, text string
	_ = json.Unmarshal(message["method"], &method)
	_ = json.Unmarshal(params["sessionId"], &session)
	_ = json.Unmarshal(update["sessionUpdate"], &kind)
	_ = json.Unmarshal(content["text"], &text)
	if method != "session/update" || kind != "user_message_chunk" || session == "" || text == "" {
		return line, false
	}
	c.mu.Lock()
	preamble := c.echoPreamble[session]
	if preamble == "" || !strings.Contains(text, preamble) {
		c.mu.Unlock()
		return line, false
	}
	delete(c.echoPreamble, session)
	c.mu.Unlock()
	text = strings.Replace(text, preamble, "", 1)
	if text == "" {
		return nil, true
	}
	content["text"], _ = json.Marshal(text)
	update["content"], _ = json.Marshal(content)
	params["update"], _ = json.Marshal(update)
	message["params"], _ = json.Marshal(params)
	encoded, err := json.Marshal(message)
	if err != nil {
		return line, false
	}
	return append(encoded, '\n'), true
}

// appendTurn adds text to a session's in-progress turn narrative, keeping the TAIL when it
// overflows the entry cap — conclusions land last.
func (c *acpControl) appendTurn(session, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turnActive[session] {
		return // startup/auth/status output outside an admitted prompt is not conversation history
	}
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
	if !c.turnActive[s] {
		return s, "", true // tool/status chatter outside an admitted prompt is not conversation history
	}
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
// assistant turns; user prompts are head-bounded here), retaining the provider that actually owned
// the turn, and evicts the oldest entries past the session budget. Called with c.mu held.
func (c *acpControl) appendHistoryLocked(session, provider, role, text string) {
	if len(text) > historyEntryBytes {
		text = text[:historyEntryBytes] // the HEAD — a long user prompt leads with the ask
	}
	h := c.history[session]
	if h == nil {
		h = &sessHistory{}
		c.history[session] = h
	}
	h.entries = append(h.entries, histEntry{provider: provider, role: role, text: text})
	h.size += len(text)
	for h.size > c.carryBytes && len(h.entries) > 1 {
		h.size -= len(h.entries[0].text)
		h.entries = h.entries[1:]
		h.evicted = true
	}
}

// preambleLocked renders a session's carried history as the plain-text context block prepended
// to the first prompt after a re-create — labeled and honest about its fidelity. When omitCurrentUser
// is true, the latest USER entry is omitted wherever it sits: it is the message being (re)sent, and
// a provider switch can append visible partial assistant output after it. Called with c.mu held.
func (c *acpControl) preambleLocked(session string, omitCurrentUser bool) string {
	h := c.history[session]
	if h == nil {
		return ""
	}
	omitUser := -1
	if omitCurrentUser {
		for i := len(h.entries) - 1; i >= 0; i-- {
			if h.entries[i].role == "user" {
				omitUser = i
				break
			}
		}
	}
	entries := make([]histEntry, 0, len(h.entries))
	for i, e := range h.entries {
		if i != omitUser {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	providers := make([]string, 0, len(entries))
	lastProvider := ""
	for _, e := range entries {
		if e.provider != "" && e.provider != lastProvider {
			providers = append(providers, e.provider)
			lastProvider = e.provider
		}
	}
	origin := "the previous session"
	if len(providers) > 0 {
		origin = strings.Join(providers, ", then ")
	}
	fmt.Fprintf(&b, "[coop] This thread continues a conversation carried over from %s — the session was re-created (provider switch or lost transcript). The context below is best-effort: message text plus one-line tool narration; tool payloads/results are not included, so re-read anything you need to rely on. Continue the conversation naturally.\n\n--- conversation so far ---\n", origin)
	if h.evicted {
		b.WriteString("(…earlier context omitted — the carried history hit its budget…)\n\n")
	}
	lastProvider = ""
	for _, e := range entries {
		if e.provider != "" && e.provider != lastProvider {
			fmt.Fprintf(&b, "[provider] %s\n\n", e.provider)
			lastProvider = e.provider
		}
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

// carriedPreamble returns Coop's injected first text block from an admitted prompt. Matching the full
// labeled block, rather than a marker or provider name, keeps user-authored chunks untouched.
func carriedPreamble(line []byte) string {
	var message struct {
		Params struct {
			Prompt []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &message) != nil || len(message.Params.Prompt) == 0 {
		return ""
	}
	first := message.Params.Prompt[0]
	if first.Type != "text" || !strings.HasPrefix(first.Text, "[coop] This thread continues") {
		return ""
	}
	return first.Text
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

// accountLimitKey keeps equal profile names on different providers from sharing cooldown state.
func accountLimitKey(provider, account string) string {
	return loginTarget(provider, account)
}

// nextAccount marks cur rate-limited until `until`, then returns the next signed-in account (cyclic
// from cur) whose own limit has expired, or "" when there's no other free account right now.
func (c *acpControl) nextAccount(provider, cur string, until, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limited[accountLimitKey(provider, cur)] = until
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
		if t, ok := c.limited[accountLimitKey(provider, cand)]; !ok || !t.After(now) {
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
// the adapter's offered values), prepends coop's selectors, and caches the result
// per session so a set_config_option response can echo the full set. Native options pass through as
// raw JSON so any fields coop doesn't model survive. When the adapter emits its models in a `models`
// field instead of a native `model` configOption and no `model` option is present, Coop
// synthesizes one from that field so the editor renders a coop-owned model dropdown.
func (c *acpControl) rewriteConfigOptions(raw, models json.RawMessage, sid string, replay bool) json.RawMessage {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return raw
	}
	provider := c.leadProvider()
	if sid != "" {
		c.mu.Lock()
		native := c.nativeCache[sid]
		if replay && len(arr) == 0 && len(models) == 0 && native.Provider == provider {
			raw = append(json.RawMessage(nil), native.Options...)
			models = append(json.RawMessage(nil), native.Models...)
			_ = json.Unmarshal(raw, &arr)
		} else {
			c.nativeCache[sid] = nativeOptionCache{
				Provider: provider,
				Options:  append(json.RawMessage(nil), raw...),
				Models:   append(json.RawMessage(nil), models...),
			}
		}
		c.mu.Unlock()
	}
	// On a PRESET the preset's lead ladder owns the model — show the box's truth, never coop's
	// launch-time model over it.
	sel := c.selection()
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
			if sel.Preset == "" && model != "" && optionHasValue(head.Options, model) {
				item = withField(item, "currentValue", model) // default to coop's model; still switchable
			}
		}
		// On a preset every native knob (model, effort, fast, …) is inert — the preset's ladder
		// and roles own them — so only coop's Preset selector renders. Leaving the preset brings them
		// back on the next box truth (the restart's config_option_update).
		if sel.Preset != "" {
			continue
		}
		out = append(out, item)
	}
	// ACP models shape: no native model option, but a models field carrying the choices. Synthesize coop's
	// own `model` select and latch the lead so fromEditor/sessionReady route via session/set_model.
	if !hasModel && sel.Preset == "" {
		if synth := c.synthModelOption(models, sel.Preset, model); synth != nil {
			out = append(out, synth)
			c.cacheModels(parseGeminiModels(models)) // free refresh from the generic ACP `models` shape
			c.mu.Lock()
			c.leadUsesSetModel = true
			c.mu.Unlock()
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	c.mu.Lock()
	if sid != "" {
		c.cached[sid] = b
	}
	c.mu.Unlock()
	return b
}

// cacheModels writes the lead's models to its per-agent cache during a normal ACP session —
// the free, opportunistic refresh that keeps `coop models` live from native options/models at zero
// extra cost (the session/new models are already parsed here). Best-effort: an empty list or
// a write error is ignored, so the plain `coop models` just falls back to the static list.
func (c *acpControl) cacheModels(models []modelInfo) {
	_ = writeModelsCache(c.cfg, c.lead, models)
}

// coopOptions builds the toolbar: plain sessions show Preset, Provider (omitted for a fusion
// governor — see fusionLadderGuard), and Account; an active preset shows only Preset because its
// ladder owns the full lead target.
func (c *acpControl) coopOptions() []json.RawMessage {
	c.mu.Lock()
	sel, lead, creds, fusion := c.sel, c.lead, c.creds, c.fusion
	sel = normalizeACPSelection(sel)
	c.sel = sel
	c.mu.Unlock()
	var presetTarget agents.Target
	if sel.Preset != "" {
		if rot := c.presetRotation(); rot != nil {
			c.mu.Lock()
			presetTarget = rot.active()
			c.mu.Unlock()
		}
	}
	var out []json.RawMessage
	ps := "none"
	if sel.Preset != "" {
		ps = sel.Preset
	}
	popts := make([]acpOption, 0, len(c.presets)+1)
	popts = append(popts, acpOption{Value: "none", Name: "None", Description: "No preset — the plain lead"})
	for _, p := range c.presets {
		option := acpOption{Value: p, Name: p, Description: "Run under preset " + p + " (its lead ladder + roles)"}
		if p == sel.Preset && presetTarget.Provider != "" {
			option.Name += " · " + displayName(presetTarget.Provider)
			if presetTarget.Model != "" {
				option.Name += " · " + presetTarget.Model
			}
			option.Description = "Active target: " + presetTarget.String()
		}
		popts = append(popts, option)
	}
	out = append(out, marshalSelect(coopPresetID, "Preset",
		"Orchestration recipe — its ladder owns provider, model, effort, account, and roles", ps, popts))
	if sel.Preset != "" {
		return out
	}
	if !fusion {
		others := c.spawnableProviders(lead)
		opts := make([]acpOption, 0, len(others)+1)
		opts = append(opts, acpOption{Value: lead, Name: displayName(lead), Description: "The current provider"})
		for _, p := range others {
			opts = append(opts, acpOption{Value: p, Name: displayName(p),
				Description: "Switch the plain session to " + displayName(p) + " — context carried best-effort"})
		}
		out = append(out, marshalSelect(coopProviderID, "Provider",
			"Who runs a plain session — switching re-creates the thread and carries context best-effort", lead, opts))
	}
	acct := "auto"
	if sel.Account != "" {
		acct = sel.Account
	}
	aopts := make([]acpOption, 0, len(creds)+1)
	aopts = append(aopts, acpOption{Value: "auto", Name: "Auto", Description: "Use the provider default or automatic account fan-out"})
	for _, cr := range creds {
		aopts = append(aopts, acpOption{Value: cr, Name: cr, Description: "Switch to account " + cr + " — the conversation is preserved"})
	}
	out = append(out, marshalSelect(coopAccountID, "Account",
		"The plain lead's login — switching is transparent (shared session store)", acct, aopts))
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

// selectorSelection applies one dropdown value. Presets own the complete active selection, so
// Provider and Account writes while one is active are acknowledged as refused no-ops. Invalid or
// unspawnable plain values likewise return the unchanged selection.
func (c *acpControl) selectorSelection(configID, value string) (next acpSelection, recognized bool) {
	c.mu.Lock()
	next, lead, creds, fusion := c.sel, c.lead, slices.Clone(c.creds), c.fusion
	next = normalizeACPSelection(next)
	c.mu.Unlock()
	switch configID {
	case coopProviderID:
		if next.Preset != "" {
			return next, true
		}
		if value == lead || fusion || !agents.Valid(value) || len(accountsFor(c.cfg, value)) == 0 {
			return next, true
		}
		next.Provider = value
		if next.Account != "" && !slices.Contains(accountsFor(c.cfg, value), next.Account) {
			next.Account = ""
		}
		return next, true
	case coopAccountID:
		if next.Preset != "" {
			return next, true
		}
		if value == "auto" {
			next.Account = ""
			return next, true
		}
		if !slices.Contains(creds, value) {
			return next, true
		}
		next.Account = value
		return next, true
	case coopPresetID:
		if value == "none" {
			if next.Preset != "" {
				next = acpSelection{Provider: lead}
			} else {
				next.Preset = ""
			}
			return next, true
		}
		if !slices.Contains(c.presets, value) {
			return next, true
		}
		return acpSelection{Preset: value}, true
	}
	return next, false
}

func (c *acpControl) presetLead(name, fallback string) string {
	if p, err := preset.Load(c.repo, c.cfg.GlobalPresetsDir(), name); err == nil {
		return p.LeadAgent
	}
	return fallback
}

// ctrlSnapshot captures the selection state a fresh controller can't re-derive across a supervisor
// re-exec (creds/presets/accounts re-derive from cfg/repo at construction; retargetLocked recomputes
// per-lead state). Carried through a SIGHUP reload so the toolbar/lead/model survive the binary swap.
type ctrlSnapshot struct {
	Selection        acpSelection                 `json:"selection"`
	AutoAccount      string                       `json:"auto_account,omitempty"`
	AuthFailed       map[string]bool              `json:"auth_failed,omitempty"`
	Limited          map[string]time.Time         `json:"limited,omitempty"`
	RotationLimited  map[string]time.Time         `json:"rotation_limited,omitempty"`
	PlainTargets     map[string]targetPreference  `json:"plain_targets,omitempty"`
	Cached           map[string]json.RawMessage   `json:"cached_options,omitempty"`
	NativeCache      map[string]nativeOptionCache `json:"native_options,omitempty"`
	Recreate         map[string]bool              `json:"recreate_sessions,omitempty"`
	Lead             string                       `json:"lead"`
	Model            string                       `json:"model"`
	Target           agents.Target                `json:"target"`
	LeadUsesSetModel bool                         `json:"lead_uses_set_model"`
}

// snapshot captures the selection state under the lock.
func (c *acpControl) snapshot() ctrlSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	var rotationLimited map[string]time.Time
	if c.rot != nil {
		rotationLimited = cloneStringTimeMap(c.rot.limited)
	}
	return ctrlSnapshot{
		Selection: normalizeACPSelection(c.sel), AutoAccount: c.autoAccount, AuthFailed: cloneStringBoolMap(c.authFailed),
		Limited: cloneStringTimeMap(c.limited), RotationLimited: rotationLimited,
		PlainTargets: cloneTargetPreferences(c.plainTargets), Cached: cloneRawMessageMap(c.cached), NativeCache: cloneNativeOptionCache(c.nativeCache),
		Recreate: cloneStringBoolMap(c.recreate),
		Lead:     c.lead, Model: c.model, Target: c.target, LeadUsesSetModel: c.leadUsesSetModel,
	}
}

// restore re-applies a snapshot into a fresh controller: retargetLocked re-derives the per-lead
// state for the restored lead (and clears the model/set-model latch), then the normalized snapshot's
// own sel/model/leadUsesSetModel are set on top.
func (c *acpControl) restore(s ctrlSnapshot) {
	s.Selection = normalizeACPSelection(s.Selection)
	c.mu.Lock()
	c.retargetLocked(s.Lead)
	if s.Target.Provider == "" {
		s.Target = agents.Target{Provider: s.Lead, Model: s.Model, Effort: c.cfg.EffortFor(s.Lead)}
	}
	c.lead, c.sel, c.autoAccount, c.model, c.target, c.leadUsesSetModel = s.Lead, s.Selection, s.AutoAccount, s.Model, s.Target, s.LeadUsesSetModel
	c.plainTargets = cloneTargetPreferences(s.PlainTargets)
	if c.plainTargets == nil {
		c.plainTargets = map[string]targetPreference{}
	}
	if _, ok := c.plainTargets[s.Lead]; !ok {
		c.plainTargets[s.Lead] = targetPreference{Model: s.Model, Effort: s.Target.Effort}
	}
	c.cached = cloneRawMessageMap(s.Cached)
	if c.cached == nil {
		c.cached = map[string]json.RawMessage{}
	}
	c.nativeCache = cloneNativeOptionCache(s.NativeCache)
	if c.nativeCache == nil {
		c.nativeCache = map[string]nativeOptionCache{}
	}
	c.recreate = cloneStringBoolMap(s.Recreate)
	if c.recreate == nil {
		c.recreate = map[string]bool{}
	}
	c.authFailed = cloneStringBoolMap(s.AuthFailed)
	if c.authFailed == nil {
		c.authFailed = map[string]bool{}
	}
	c.limited = cloneStringTimeMap(s.Limited)
	if c.limited == nil {
		c.limited = map[string]time.Time{}
	}
	c.mu.Unlock()

	// A preset rotation is rebuilt after re-exec. Resume on the exact target that was active at the
	// snapshot instead of silently resetting to rung zero; if the preset changed and no longer contains
	// that target, its new first rung remains authoritative.
	if s.Selection.Preset != "" {
		if rot := c.presetRotation(); rot != nil {
			c.mu.Lock()
			rot.limited = cloneStringTimeMap(s.RotationLimited)
			if rot.limited == nil {
				rot.limited = map[string]time.Time{}
			}
			for i, target := range rot.targets {
				if target.String() == s.Target.String() {
					rot.idx = i
					break
				}
			}
			c.mu.Unlock()
		}
	}
}

func cloneStringBoolMap(src map[string]bool) map[string]bool {
	if src == nil {
		return nil
	}
	dst := make(map[string]bool, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneStringTimeMap(src map[string]time.Time) map[string]time.Time {
	if src == nil {
		return nil
	}
	dst := make(map[string]time.Time, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneTargetPreferences(src map[string]targetPreference) map[string]targetPreference {
	if src == nil {
		return nil
	}
	dst := make(map[string]targetPreference, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneRawMessageMap(src map[string]json.RawMessage) map[string]json.RawMessage {
	if src == nil {
		return nil
	}
	dst := make(map[string]json.RawMessage, len(src))
	for key, value := range src {
		dst[key] = append(json.RawMessage(nil), value...)
	}
	return dst
}

func cloneNativeOptionCache(src map[string]nativeOptionCache) map[string]nativeOptionCache {
	if src == nil {
		return nil
	}
	dst := make(map[string]nativeOptionCache, len(src))
	for key, value := range src {
		value.Options = append(json.RawMessage(nil), value.Options...)
		value.Models = append(json.RawMessage(nil), value.Models...)
		dst[key] = value
	}
	return dst
}

// leadProvider is the current lead's provider (the one the active box runs), read under the lock —
// the warm pool warms the OTHER signed-in providers around it.
func (c *acpControl) leadProvider() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lead
}

// spawnableProviders lists the OTHER registered providers with at least one signed-in account —
// the ones a provider switch could actually spawn. The current lead is excluded (its accounts
// are offered by the Account selector).
func (c *acpControl) spawnableProviders(lead string) []string {
	var out []string
	for _, p := range agents.Names() {
		if p != lead && len(accountsFor(c.cfg, p)) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// synthModelOption builds a Coop-owned `model` select from an adapter's session/new `models` field
// ({availableModels:[{modelId,name,description?}], currentModelId}). Returns nil when the field
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

// sessionReady returns the provider-owned ordered settings for the complete active target. They are
// re-applied after every new/load/recreate because adapters reset session state at those boundaries.
func (c *acpControl) sessionReady(sid string) [][]byte {
	c.mu.Lock()
	target := c.target
	c.mu.Unlock()
	if target.Provider == "" {
		return nil
	}
	a, ok := agents.Get(target.Provider)
	if !ok {
		return nil
	}
	var msgs [][]byte
	for _, setting := range a.ACPSessionSettings(target) {
		switch setting.Method {
		case agents.ACPSetConfigOption:
			msgs = append(msgs, c.setConfig(sid, setting.ConfigID, setting.Value))
		case agents.ACPSetModel:
			msgs = append(msgs, c.setModel(sid, setting.Value))
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
	reqID := acpproxy.InjectPrefix + itoa(c.nextID)
	c.mu.Unlock()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"method":  "session/set_config_option",
		"params":  map[string]any{"sessionId": sid, "configId": id, "value": value},
	}
	b, _ := json.Marshal(req)
	return append(b, '\n')
}

// setModel builds the ACP session/set_model request ({sessionId, modelId}) used by adapters that
// expose a `models` result instead of a native model config option. InjectPrefix hides forced replies.
func (c *acpControl) setModel(sid, model string) []byte {
	c.mu.Lock()
	c.nextID++
	reqID := acpproxy.InjectPrefix + itoa(c.nextID)
	c.mu.Unlock()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"method":  "session/set_model",
		"params":  map[string]any{"sessionId": sid, "modelId": model},
	}
	b, _ := json.Marshal(req)
	return append(b, '\n')
}

// injectedResponse surfaces a rejected provider setting in the affected editor session. Successful
// force-sets stay invisible; a failure cannot silently leave the adapter on the wrong target.
func (c *acpControl) injectedResponse(request, response []byte) []byte {
	var req struct {
		Params struct {
			SessionID string `json:"sessionId"`
			ConfigID  string `json:"configId"`
			Value     string `json:"value"`
			ModelID   string `json:"modelId"`
		} `json:"params"`
	}
	var reply struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(request, &req) != nil || json.Unmarshal(response, &reply) != nil {
		return nil
	}
	if req.Params.SessionID == "" || len(reply.Error) == 0 || string(reply.Error) == "null" {
		return nil
	}
	var rpcError struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(reply.Error, &rpcError)
	detail := strings.TrimSpace(rpcError.Message)
	if detail == "" {
		detail = strings.TrimSpace(string(reply.Error))
	}
	setting := req.Params.ConfigID + "=" + req.Params.Value
	if req.Params.ModelID != "" {
		setting = "model=" + req.Params.ModelID
	}
	text := fmt.Sprintf("Coop could not apply ACP target setting %s: %s. The adapter may be running a different target.", setting, detail)
	return sessionMessage(req.Params.SessionID, text)
}

func sessionMessage(sid, text string) []byte {
	message := map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sid,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
			},
		},
	}
	b, _ := json.Marshal(message)
	return append(b, '\n')
}

// fromEditor intercepts the editor's set of coop's own selector: it updates the selection and asks
// the proxy to restart the box on the new plain lead or preset, replying to the editor itself (the
// adapter never sees coop-owned selectors). Provider/Account writes during a preset are acknowledged as no-ops.
// It also translates a set of coop's SYNTHESIZED model dropdown into the adapter's session/set_model.
// The translated request keeps the editor's request id and follows the normal proxy path, so success
// is not acknowledged until the provider accepts it. Native option sets (a real adapter
// model/effort/fast option) return handled=false so they pass through to the adapter unchanged.
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
	if h.Method == "authenticate" || h.Method == "logout" {
		provider, account := c.authenticationTarget()
		message := fmt.Sprintf("coop manages authentication per account. Re-authenticate with: coop login %s", loginTarget(provider, account))
		return true, rpcErrorResponse(h.ID, -32601, message), nil, false
	}
	// Remember each session's in-flight prompt so a rate-limit rotation/wait can re-send it, and
	// record its text in the carried history. If the session was just re-created (a provider
	// switch), this prompt is the first into the fresh thread — rewrite it with the history
	// preamble (the proxy forwards the rewrite through the normal path, editor id intact).
	if h.Method == "session/prompt" && h.Params.SessionID != "" && len(h.ID) > 0 {
		sid := h.Params.SessionID
		raw := append([]byte(nil), line...)
		var wrapped []byte
		c.mu.Lock()
		c.closeProviderTurnLocked(sid, c.lead)
		text := promptText(line)
		if text != "" {
			c.appendHistoryLocked(sid, c.lead, "user", text)
		}
		if c.needPreamble[sid] {
			delete(c.needPreamble, sid)
			if pre := c.preambleLocked(sid, text != ""); pre != "" {
				wrapped = wrapPromptLine(raw, pre)
			}
		}
		c.lastPrompt[sid] = raw
		c.mu.Unlock()
		return false, nil, wrapped, false
	}
	// On a preset the native knobs are hidden (rewriteConfigOptions drops them — the preset owns
	// them), but an editor may still SET one: Zed re-applies its persisted default_config_options
	// to every new session. Forwarding would silently override the preset's pick in the box, so
	// swallow it and ack with the real (coop-only) option set.
	if h.Method == "session/set_config_option" && !coopOwnedIDs[h.Params.ConfigID] {
		if sel := c.selection(); sel.Preset != "" {
			return true, c.ackOptions(h.ID, h.Params.SessionID), nil, false
		}
		// A synthesized model is committed from the translated session/set_model response below,
		// not from the editor request itself.
		if h.Params.ConfigID == "model" {
			c.mu.Lock()
			synth := c.leadUsesSetModel
			c.mu.Unlock()
			if synth {
				return c.setModelFromEditor(h.ID, h.Params.SessionID, h.Params.Value)
			}
		}
		field := ""
		switch h.Params.ConfigID {
		case "model":
			field = "model"
		case "effort", "reasoning_effort":
			field = "effort"
		}
		if field != "" && len(h.ID) > 0 {
			c.registerNativeTarget(string(h.ID), field, h.Params.Value, h.Params.SessionID, false)
		}
	}
	next, recognized := c.selectorSelection(h.Params.ConfigID, h.Params.Value)
	if !recognized {
		return false, nil, nil, false
	}
	c.mu.Lock()
	// Only a REAL change restarts. Editors (Zed) apply default_config_options at startup by SETTING
	// a dropdown to the value it's already on; restarting on that no-op would respawn the box before
	// the session has any transcript, so the replayed session/load fails "Resource not found" and the
	// conversation is lost before it begins. A no-op (or a refused value) just re-acks.
	c.sel = normalizeACPSelection(c.sel)
	changed := next != c.sel
	if h.Params.ConfigID == coopAccountID && (h.Params.Value == "auto" || next.Account == h.Params.Value) {
		if h.Params.Value == "auto" && c.autoAccount != "" {
			changed = true
		}
		c.autoAccount = ""
		c.authFailed = map[string]bool{} // an explicit account action is a deliberate retry boundary
	}
	if changed {
		c.sel = next
		c.rot, c.rotFor = nil, acpSelection{}
	}
	c.mu.Unlock()
	if changed {
		// Resolve now so the ack below renders the new effective provider and its account menu,
		// rather than visibly flipping back until the respawn's config_option_update arrives.
		_, _, _ = c.spawnTarget()
		c.mu.Lock()
		// The restart kills any in-flight turn mid-stream. Arm the same transparent resend a
		// rate-limit rotation uses so the turn completes on the new selected target.
		c.armPromptResendsLocked(fmt.Sprintf("switch to %+v", next))
		c.mu.Unlock()
	}
	// Ack with the full option set, the coop dropdowns showing the CURRENT value. The cache was
	// captured at session/new with the old currentValue, so echoing it verbatim would revert the
	// editor's dropdown; rebuild them fresh.
	return true, c.ackOptions(h.ID, h.Params.SessionID), nil, changed
}

func (c *acpControl) authenticationTarget() (provider, account string) {
	c.mu.Lock()
	provider = c.lead
	sel := normalizeACPSelection(c.sel)
	account = sel.Account
	if sel.Preset != "" {
		account = c.target.Account()
	} else if account == "" {
		account = c.autoAccount
	}
	if account == "" && len(c.accounts) > 0 {
		account = c.accounts[0]
	}
	c.mu.Unlock()
	if account == "" {
		account = c.cfg.ActiveProfile(provider)
	}
	return provider, account
}

func rpcErrorResponse(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		return nil
	}
	response := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
	encoded, _ := json.Marshal(response)
	return append(encoded, '\n')
}

// ackOptions builds the reply to an editor set_config_option coop answers itself — the full
// refreshed option set (fresh coop dropdowns, natives per the current selection), re-cached so
// the next refresh starts from what the editor now shows.
func (c *acpControl) ackOptions(id json.RawMessage, sid string) []byte {
	refreshed := c.refreshSetup(sid)
	result := map[string]json.RawMessage{}
	if len(refreshed) > 0 {
		result["configOptions"] = refreshed
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	b, _ := json.Marshal(out)
	return append(b, '\n')
}

// setModelFromEditor handles a set of coop's synthesized model dropdown: it rewrites the request to
// session/set_model while retaining the editor's id. The normal proxy response path therefore records
// and acknowledges the pick only after adapter success. No box restart — this is a live switch.
func (c *acpControl) setModelFromEditor(id json.RawMessage, sid, value string) (bool, []byte, []byte, bool) {
	if len(id) == 0 || sid == "" || value == "" {
		return true, rpcErrorResponse(id, -32602, "session/set_config_option requires a session and model"), nil, false
	}
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "session/set_model",
		"params":  map[string]any{"sessionId": sid, "modelId": value},
	}
	encoded, _ := json.Marshal(request)
	c.registerNativeTarget(string(id), "model", value, sid, true)
	return false, nil, append(encoded, '\n'), false
}

// translatedModelResponse converts a provider's session/set_model response back into the response
// shape expected for the editor's original session/set_config_option request. Provider errors remain
// errors; successful replies carry the full refreshed option set at the accepted model.
func (c *acpControl) translatedModelResponse(line []byte, target nativeTargetResult) []byte {
	var response map[string]json.RawMessage
	if json.Unmarshal(line, &response) != nil {
		return line
	}
	if target.success {
		result := map[string]json.RawMessage{}
		if refreshed := c.refreshModelAck(target.change.session, target.accepted); len(refreshed) > 0 {
			result["configOptions"] = refreshed
		}
		encoded, _ := json.Marshal(result)
		response["result"] = encoded
		delete(response, "error")
	} else if rawError := response["error"]; len(rawError) > 0 && string(rawError) != "null" {
		var rpcError map[string]json.RawMessage
		if json.Unmarshal(rawError, &rpcError) == nil {
			var detail string
			_ = json.Unmarshal(rpcError["message"], &detail)
			detail = strings.TrimSpace(detail)
			if detail == "" {
				detail = strings.TrimSpace(string(rawError))
			}
			message := fmt.Sprintf("Model %s was rejected: %s.", target.change.value, detail)
			if target.accepted != "" {
				message += " Restored " + target.accepted + "."
			}
			rpcError["message"], _ = json.Marshal(message)
			response["error"], _ = json.Marshal(rpcError)
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return line
	}
	return append(encoded, '\n')
}

// modelRestartSuggested recognizes an adapter's structured statement that a valid model requires a
// fresh session (for example, Grok models backed by a different agent type). Free-form error prose is
// deliberately insufficient: both the incompatibility code and recovery suggestion must be explicit.
func modelRestartSuggested(raw json.RawMessage) bool {
	incompatible, restart := false, false
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	walkJSONStrings(value, "", func(key, value string) {
		switch compactJSONName(key) {
		case "code":
			incompatible = incompatible || compactJSONName(value) == "modelswitchincompatibleagent"
		case "suggestion":
			restart = restart || compactJSONName(value) == "startnewsession"
		}
	})
	return incompatible && restart
}

// refreshModelAck rebuilds the cached option array with fresh coop dropdowns and the model
// option's currentValue set to value — the ack a synthesized-model set echoes so the editor's
// dropdown keeps the pick.
func (c *acpControl) refreshModelAck(sid, value string) json.RawMessage {
	refreshed := c.refreshSetup(sid)
	var arr []json.RawMessage
	if json.Unmarshal(refreshed, &arr) != nil {
		return refreshed
	}
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
		return refreshed
	}
	if sid != "" {
		c.mu.Lock()
		c.cached[sid] = b
		c.mu.Unlock()
	}
	return b
}

// refreshSetup re-renders from provider-tagged native truth. The replay flag permits an adapter that
// omits metadata on session/load to reuse only the same provider's prior controls; a cross-provider
// switch therefore cannot echo the source provider's model menu.
func (c *acpControl) refreshSetup(sid string) json.RawMessage {
	return c.rewriteConfigOptions(json.RawMessage("[]"), nil, sid, true)
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
