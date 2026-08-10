package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/liveprocess"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

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

// acpHost builds the real acpctl.Host: the rotation/fusion-council/models-cache/wait policy the ACP
// control needs but cannot own itself (see internal/acpctl's Host doc). Called by cmdACP for
// production, and reused by this package's own tests so they construct a control with real (not
// faked) behavior.
func acpHost() acpctl.Host {
	return acpctl.Host{
		ExpandLadder: expandLadder,
		AccountsFor:  accountsFor,
		ResolveFusionCouncil: func(governor string, peers []agents.Target, p *preset.Preset, available []string, reachable []agents.Target) (acpctl.FusionCouncil, error) {
			fc, err := resolveACPFusionCouncil(governor, peers, p, available, reachable)
			if err != nil {
				return acpctl.FusionCouncil{}, err
			}
			return acpctl.FusionCouncil{Peers: fc.Peers, Members: fc.Members, UnavailableRoles: fc.UnavailableRoles}, nil
		},
		WriteModelsCache: writeModelsCache,
		WaitUntilWall:    waitUntilWall,
	}
}

// acpCommand maps a validated agent to its ACP adapter command inside the box.
func acpCommand(cfg *config.Config, tool string) []string {
	if ag, ok := agents.Get(tool); ok {
		return ag.ACP(cfg)
	}
	return nil
}

// cmdACP runs the box as an ACP agent over stdio: the repo mounts at its real
// host path (so the editor's absolute paths resolve, and the session history
// matches `coop`/`coop loop` — see resolveWorkdir) and no tty is allocated. The
// explicit Workdir forces the real path even if COOP_WORKDIR is set.
//
// `coop acp fusion [governor]` fronts the governor's adapter as a normal ACP
// agent (so Zed drives it like any other) but wired for fusion: it consults its
// peers read-only and synthesizes (see cmdFusion). Add one Zed agent_servers
// entry per governor to switch which model leads.
// defaultACPProvider picks the provider for a bare `coop acp` (no positional target, no preset
// lead): the first signed-in agent, or "" when none is signed in. ACP-only — the editor toolbar's
// provider dropdown can switch it live, so an implicit default is safe here where `coop claude`/
// `coop loop` are deliberately strict (no dropdown to correct a wrong guess).
func defaultACPProvider(cfg *config.Config) string {
	if authed := box.AuthedAgents(cfg); len(authed) > 0 {
		return authed[0]
	}
	return ""
}

func (a *app) cmdACP(args []string) (int, error) {
	// The ACP proxy is ALWAYS in the path: it's coop's control point for the editor session —
	// restart resilience, plus rewriting the session so coop owns the toolbar (yolo, model default,
	// coop's plain/preset toolbar selectors). The OUTER process validates the args (fail fast), then
	// supervises; the INNER (COOP_ACP_INNER=1) runs the box.
	inner := args // the args the supervisor re-execs as `coop acp <inner>`; the inner re-parses them
	innerProcess := os.Getenv("COOP_ACP_INNER") != ""
	peerVals, args, err := extractPeer(args)
	if err != nil {
		return 2, err
	}
	// Resolve the --peer peers HERE, before the outer/inner split — so an editor's
	// agent_servers entry with a bad peer (unknown/unauthed, or an @account) fails fast in the
	// OUTER process, not silently later inside the box.
	peers, err := a.resolvePeers("--peer", peerVals)
	if err != nil {
		return 2, err
	}
	allPeers := slices.Clone(peers) // Re-evaluate self exclusion after every ACP provider rotation.
	// The positional who-runs slot pins the session: a TARGET (provider[:model][/effort][@account],
	// so an editor's agent_servers entry runs ["acp","claude:opus@work"]) OR a PRESET NAME (routing +
	// role wiring; its lead is the agent — or governor, under fusion). Parsed BEFORE the inner
	// env-override block so a preset-rotation rung (COOP_ACP_TARGET) still wins over the launch-time
	// model/account. fusion is a keyword (a governor slot follows), not itself a provider/preset.
	model, profile, effort := "", "", ""
	tool, toolSet := "", false // no implicit default; an empty tool falls to the required-provider error below
	governor := ""
	presetName := ""
	consumed := 0 // positional tokens accounted for (the agent, plus a governor under fusion)
	isFusion := len(args) > 0 && args[0] == "fusion"
	// takeWho classifies a positional who slot: a target folds its model/effort/account in and sets
	// the provider; a preset name is captured for loadRunPreset below. Shared by the agent and the
	// fusion-governor slot so both accept a preset.
	takeWho := func(who string, provider *string) error {
		if !isTargetHead(who) {
			presetName = who
			return nil
		}
		t, terr := agents.ParseTarget(who)
		if terr != nil {
			return terr
		}
		*provider = t.Provider
		toolSet = true
		if terr := foldTarget(t, &model, &profile); terr != nil {
			return terr
		}
		effort = t.Effort
		return nil
	}
	switch {
	case isFusion:
		consumed = 1
		governor, toolSet = "", false // named explicitly (or via a preset lead) — no implicit default
		if len(args) > 1 {
			if terr := takeWho(args[1], &governor); terr != nil {
				return 2, terr
			}
			consumed = 2
		}
	case len(args) > 0:
		if terr := takeWho(args[0], &tool); terr != nil {
			return 2, terr
		}
		consumed = 1
	}
	// Reject leftover tokens rather than silently ignore them (loop/fork do the same) — the ACP
	// adapter takes no extra args, so `coop acp claude foo`/`--nope` is a mistake worth surfacing.
	if leftover := args[consumed:]; len(leftover) > 0 {
		return 2, fmt.Errorf("coop acp: unexpected argument %q (usage: coop acp <agent>[:model][/effort][@account] | fusion <agent>[:model][/effort][@account] | <preset>)", leftover[0])
	}
	// A running ACP session can switch its credential/preset/provider via coop's selector; the
	// supervisor re-execs the inner box with the resolved spawn target in the env
	// (COOP_ACP_TARGET, wire grammar) plus the preset whose roles mount (COOP_ACP_PRESET). The
	// target is the COMPLETE spawn intent — provider, model, account are taken from it verbatim
	// (empty slots mean the provider's defaults), so a provider switch or a cross-provider preset
	// rung fully replaces the launch identity instead of leaking the old lead's model/account.
	if innerProcess {
		if ps, selected := os.LookupEnv("COOP_ACP_PRESET"); selected {
			presetName = ps
		}
		if tv := os.Getenv("COOP_ACP_TARGET"); tv != "" {
			t, terr := agents.ParseTarget(tv)
			if terr != nil {
				return 2, fmt.Errorf("COOP_ACP_TARGET: %v", terr)
			}
			tool, toolSet = t.Provider, true
			if isFusion {
				governor = t.Provider // under Fusion the same switch retargets the governor
			}
			model, effort, profile = t.Model, t.Effort, t.Account()
		}
	}
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	var council fusionCouncil
	if isFusion {
		governor = presetLeadAgent(p, governor, toolSet)
		if governor == "" {
			return 2, errors.New("coop acp fusion: name the governor — coop acp fusion <agent> (or a preset name, whose lead governs)")
		}
		if !fusion.Valid(governor, agents.Names()) {
			return 2, fmt.Errorf("unknown governor %q — use %s", governor, agentChoices())
		}
		tool = governor
	} else {
		tool = presetLeadAgent(p, tool, toolSet)
		// A bare `coop acp` (no provider, no preset lead) defaults to the first signed-in provider
		// instead of erroring: the editor toolbar's provider dropdown can switch it live, so an
		// implicit default is safe HERE — unlike `coop claude`/`coop loop`, which stay strict since
		// there's no dropdown to correct a wrong guess. Nothing signed in falls through to the error.
		if tool == "" {
			tool = defaultACPProvider(a.cfg)
		}
	}
	if !agents.Valid(tool) {
		return 2, noProviderErr("acp")
	}
	// Fail a bad credential fast, in the outer process, before spawning anything (the inner's
	// applyOneOff does the real selection).
	if profile != "" && !slices.Contains(box.EffectiveProfiles(a.cfg, tool), profile) {
		return 2, fmt.Errorf("%s has no account %q — sign in first: coop login %s@%s", tool, profile, tool, profile)
	}
	if innerProcess {
		a.applyPreset(p, tool)
	} else {
		// The supervisor owns a rotation, not the preset's declared first rung. Keep the config's
		// credential truth intact while validating and expanding reachable targets; each inner
		// child receives and applies the exact concrete target through COOP_ACP_TARGET.
		a.preset = p
	}
	if isFusion {
		var reachable []agents.Target
		if p != nil {
			reachable, err = expandLadder(a.cfg, p.LeadAgent, p.LeadLadder)
			if err != nil {
				return 2, err
			}
		}
		council, err = resolveACPFusionCouncil(governor, peers, p, box.AuthedAgents(a.cfg), reachable)
		if err != nil {
			return 2, err
		}
		peers = council.Peers
	}
	// The outer process owns the editor stream via the proxy; it builds coop's control layer (the
	// toolbar rewrite + preset/plain selectors) and re-execs `coop acp <inner>` (COOP_ACP_INNER
	// set) to run the box, the current selection carried in the env. The inner falls through to box.Run.
	if !innerProcess {
		repo, _ := box.ResolveRepo(a.cfg.RepoOverride)
		ctrlModel := model
		if ctrlModel == "" {
			ctrlModel = a.cfg.ModelFor(tool)
		}
		ctrlEffort := effort
		if ctrlEffort == "" {
			ctrlEffort = a.cfg.EffortFor(tool)
		}
		// Ports the inner box will publish (.agent/project.yaml serve), reported to the editor once per
		// session. Deterministic host ports (project.HostPort), so these match what box.Run binds. Only
		// when egress is open — otherwise nothing publishes, so nothing to announce.
		var serveURLs []string
		if a.cfg.Egress == "open" {
			if pj, err := project.Load(repo); err == nil {
				for _, port := range pj.Serve.Ports {
					serveURLs = append(serveURLs, fmt.Sprintf("box :%d → http://localhost:%d", port, project.HostPort(repo, port)))
				}
			}
		}
		sel := acpctl.Selection{Account: profile, Preset: presetName}
		if toolSet {
			sel.Provider = tool
		}
		fusionPeers := make([]string, 0, len(allPeers))
		for _, peer := range allPeers {
			fusionPeers = append(fusionPeers, peer.Provider)
		}
		ctrl := acpctl.New(a.cfg, tool, ctrlModel, ctrlEffort, repo, sel, a.acpPresetNames(repo), serveURLs, isFusion, fusionPeers, acpHost())
		if a.acpSupervise != nil {
			return a.acpSupervise(inner, ctrl)
		}
		return a.cmdACPSupervise(inner, ctrl)
	}
	if err := a.applyOneOff(tool, model, profile, effort); err != nil {
		return 2, err
	}
	// Built AFTER the model selection: gemini's ACP command is its own binary and carries
	// the resolved model as a flag. tool passed agents.Valid above, so this can't miss.
	cmd := acpCommand(a.cfg, tool)
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	lead := "" // named peers (or a preset) opt the session into the second-opinion directive
	if len(peers) > 0 || a.preset != nil {
		lead = tool // a preset's routing contract mounts via ConsultLead too
	}
	// ACP speaks to an editor over stdio, not a human, so run quiet: Quiet drops coop's
	// own progress lines, and COOP_QUIET tells the box to provision the toolchain silently.
	extra := []string{"-e", "COOP_QUIET=1"}
	// Under a supervisor, give the box a deterministic identity: --cidfile lets the supervisor
	// tear it down by id even before its labels are queryable (see cmdACPSupervise's stop()).
	if cid := os.Getenv("COOP_ACP_CIDFILE"); cid != "" {
		extra = append(extra, "--cidfile", cid)
	}
	return box.Run(a.cfg, a.rt, box.RunSpec{
		// A supervisor (which reconnects the box) passes COOP_ACP_SUPERVISOR; that tags
		// the box so build/update can restart it and the supervisor can kill exactly it.
		Image: img, Repo: repo, Workdir: repo, Cmd: cmd, ForceNoTTY: true, Agent: tool, Serve: true,
		SupervisorID: os.Getenv("COOP_ACP_SUPERVISOR"), ShareACPSessions: true,
		FusionGovernor: governor, FusionMembers: council.Members, ConsultLead: lead, Peers: peers, Preset: a.preset, Quiet: true,
		ExtraArgs: extra,
		Homes:     a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

// ensureACPImage builds the box image when it is missing, so a pruned or never-built image is a
// slow first connect instead of a dead adapter. A present image is a cheap existence check and no
// build at all — this is not a freshness check, only a "can anything run" one.
//
// The build's own stdout/stdin are redirected: on this path os.Stdout is the JSON-RPC wire to the
// editor and os.Stdin carries its requests, so docker chatter there would corrupt the protocol and
// reading it would swallow the editor's initialize. ui.* already writes only to stderr, so the
// narration lands in the editor's agent log where the user can see why the first connect is slow.
func (a *app) ensureACPImage() error {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return err
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	if box.ImageExists(a.rt, img) {
		return nil
	}
	ui.Info("image %q is missing — building it now; the first connect will take a few minutes", img)
	if err := box.BuildWith(a.rt, a.cfg, repo, false, resolveVersion(), strings.NewReader(""), os.Stderr); err != nil {
		return fmt.Errorf("image %q is missing and building it failed: %w\n  build it by hand with 'coop build', then reconnect", img, err)
	}
	ui.Info("built %s — continuing", img)
	return nil
}

// cmdACPSupervise serves the editor on stdio and runs the real `coop acp <rest>` as a
// child (COOP_ACP_INNER set so the child runs the box, not another supervisor). When
// the child's container dies, acpproxy starts a new child and replays the ACP
// handshake, so the editor never sees a disconnect (see internal/acpproxy).
func (a *app) cmdACPSupervise(rest []string, ctrl *acpctl.Control) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("acp --supervise: %w", err)
	}
	inner := append([]string{"acp"}, rest...)
	// A per-supervisor id, stamped on this supervisor's boxes (coop.sup=<id>) so it can
	// kill exactly its own box(es) on teardown — not other agents' supervised boxes.
	superID, err := newSupervisorID()
	if err != nil {
		return 1, err
	}

	// A SIGHUP re-exec left us its state: restore the controller's selection and hand the proxy
	// snapshot to Run so the editor's live threads are re-established on the first (fresh) box. A
	// missing/corrupt file degrades to a fresh start (new threads still work).
	var resume *acpproxy.Snapshot
	if path := os.Getenv("COOP_ACP_RESUME_STATE"); path != "" {
		if st, rerr := acpctl.ReadResumeState(path); rerr == nil {
			ctrl.Restore(st.Ctrl)
			resume = &st.Proxy
			acpproxy.Trace("resumed from re-exec: %d session(s)", len(st.Proxy.Sessions))
		} else {
			fmt.Fprintf(os.Stderr, "coop acp: resume state unreadable (%v) — starting fresh\n", rerr)
		}
	}
	// SIGHUP → a graceful reload (re-exec the freshly-built binary in place). SIGTERM/SIGINT stay
	// STOP (below), so coop is always stoppable.
	reload := make(chan struct{}, 1)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		<-hup
		select {
		case reload <- struct{}{}:
		default:
		}
	}()

	// The image is the one thing every child needs and no child can create. Build it HERE — once, in
	// the supervisor, before the warm pool fans out and before Run's first factory call — or each
	// spawn fails on a missing image and the proxy burns its rapid-fail cap on a condition that can
	// never succeed by retrying, leaving the editor with "agent exited 5 times" and the real cause
	// buried in its agent log. One supervisor spawns every child, so this is the single-flight point.
	if err := a.ensureACPImage(); err != nil {
		return 1, err
	}

	// Keep a box warm per OTHER signed-in provider so a provider switch swaps to a hot adapter
	// (proxy replay only) instead of cold-booting one (~5s). Behind the factory: a miss cold-spawns,
	// so correctness is unaffected. COOP_ACP_WARM=0 opts out (a low-RAM escape hatch).
	warm := os.Getenv("COOP_ACP_WARM") != "0" && !ctrl.Fusion
	pool := acpctl.NewWarmPool(warm, func(provider string) (*acpproxy.Child, error) {
		return a.spawnBox(context.Background(), self, inner, superID, ctrl, agents.Target{Provider: provider}, "", true, os.Stderr)
	})
	factory := func(ctx context.Context) (*acpproxy.Child, error) {
		t, psName, ok := ctrl.SpawnTarget()
		if acpctl.BareProviderSwitch(t, psName, ok) {
			if c := pool.Checkout(t.Provider); c != nil {
				go pool.Refill(t.Provider) // keep it hot for a repeat switch
				return c, nil
			}
		}
		child, cerr := a.spawnBox(ctx, self, inner, superID, ctrl, t, psName, ok, os.Stderr)
		if acpctl.BareProviderSwitch(t, psName, ok) && cerr == nil {
			go pool.Refill(t.Provider)
		}
		return child, cerr
	}
	// Fan the other providers' boxes out in the background — the active one is spawned by Run's first
	// factory call, so startup latency is unchanged.
	if warm {
		go func() {
			others := ctrl.SpawnableProviders(ctrl.LeadProvider())
			for _, prov := range others {
				pool.Refill(prov)
			}
			acpproxy.Trace("warmed %d provider(s)", len(others))
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	defer pool.Reap() // Stop held warm boxes on any exit path; the label sweep still reaps their containers
	err = acpproxy.RunWith(ctx, os.Stdin, os.Stdout, factory, ctrl.Hooks(), acpproxy.RunOpts{Resume: resume, Reload: reload})
	// A SIGHUP reload: write the combined state to a 0600 temp file and re-exec THIS binary in place —
	// same PID + fd 0/1/2, so the editor's transport never breaks. Run's reload path already stopped
	// the box; reap the warm boxes here (execve replaces the image, so deferred reap won't run) and
	// skip the label sweep — the re-exec'd process regenerates its own superID and owns the next box.
	if snap, ok := acpproxy.ReloadSnapshot(err); ok {
		pool.Reap()
		// Sweep any box still labelled with THIS superID before exec — a warm spawn that was mid-flight
		// (reap only stops boxes already parked) would otherwise reparent to init and never be reaped
		// (the re-exec'd process uses a fresh superID). Safe here: Run already stopped the active box
		// and no new box is spawned until after exec, so nothing we need is swept.
		a.rt.KillByLabel(box.LabelSupervisor, superID)
		path, werr := acpctl.WriteResumeState(acpctl.ResumeState{Proxy: *snap, Ctrl: ctrl.Snapshot()})
		if werr != nil {
			return 1, fmt.Errorf("acp reload: %w", werr)
		}
		restoreControl, perr := acpctl.PrepareACPReload()
		if perr != nil {
			os.Remove(path)
			return 1, fmt.Errorf("acp reload: %w", perr)
		}
		if xerr := syscall.Exec(self, os.Args, append(os.Environ(), "COOP_ACP_RESUME_STATE="+path)); xerr != nil {
			restoreControl()
			os.Remove(path)
			return 1, fmt.Errorf("acp reload: exec %s: %w", self, xerr)
		}
		return 0, nil // unreachable — execve replaced the process on success
	}
	// Final teardown sweep, once, when the whole supervised session ends: a per-generation Stop
	// removes only its own box (by cidfile), so the last live generation — or a box orphaned by a
	// swap — is cleaned up here by this supervisor's id. (Doing this per-generation would kill the
	// just-spawned next box, which shares the id, fork-bombing the supervisor on the first resume.)
	a.rt.KillByLabel(box.LabelSupervisor, superID)
	if err != nil && !errors.Is(err, context.Canceled) {
		return 1, err
	}
	return 0, nil
}

func newSupervisorID() (string, error) {
	idbuf := make([]byte, 8)
	if _, err := rand.Read(idbuf); err != nil {
		return "", err
	}
	return hex.EncodeToString(idbuf), nil
}

const acpCleanupTimeout = 5 * time.Second

func cleanACPChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "COOP_ACP_INNER", "COOP_ACP_SUPERVISOR", "COOP_ACP_TARGET", "COOP_ACP_PRESET", "COOP_ACP_CIDFILE", "COOP_ACP_RESUME_STATE",
			liveprocess.ControlFDEnv, liveprocess.ProcessDirEnv, liveprocess.CleanupIDEnv, liveprocess.RevokePathEnv:
			continue
		}
		out = append(out, item)
	}
	return out
}

// spawnBox execs a `coop acp` inner box for the given spawn target and wraps it as an acpproxy.Child
// — the ONE spawn path for the live factory, warm-pool prewarm, and short-lived model probe, so each
// gets the same credentials, process isolation, and teardown.
func (a *app) spawnBox(ctx context.Context, self string, inner []string, superID string, ctrl *acpctl.Control, t agents.Target, psName string, hasTarget bool, stderr io.Writer) (*acpproxy.Child, error) {
	provider := t.Provider
	if provider == "" && ctrl != nil {
		provider = ctrl.LeadProvider()
	}
	account := t.Account()
	if account == "" && provider != "" {
		account = a.cfg.DefaultProfileOf(provider)
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	cidDir, cidPath := "", ""
	env := append(cleanACPChildEnv(os.Environ()), "COOP_ACP_INNER=1", "COOP_ACP_SUPERVISOR="+superID)
	if hasTarget {
		if ctrl != nil { // model probes use a bare provider target and need no reset/preset wait
			if psName != "" {
				ctrl.WaitForPresetRung(ctx)
			} else if acct := t.Account(); acct != "" {
				ctrl.WaitForReset(ctx, t.Provider, acct)
			}
		}
		if ctrl != nil {
			// Presence is the selection signal; an empty value explicitly clears a positional
			// launch preset in the child instead of letting the original argv resurrect it.
			env = append(env, "COOP_ACP_PRESET="+psName)
		}
		env = append(env, "COOP_ACP_TARGET="+t.String())
		acpproxy.Trace("spawn box on target=%s preset=%s", t.String(), psName)
	}
	if a.rt.SupportsCIDFile() {
		if d, derr := os.MkdirTemp("", "coop-acp-cid-"); derr == nil {
			cidDir = d
			cidPath = filepath.Join(d, "cid")
			env = append(env, "COOP_ACP_CIDFILE="+cidPath)
		}
	}
	cmd := exec.Command(self, inner...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	// Own process group: a plain Process.Kill() reaps only the inner `coop` and orphans its
	// `docker run` grandchild; killing the whole group (-pgid) reaches the run client too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := acpctl.StartACPProcess(cmd, superID); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		if cidDir != "" {
			os.RemoveAll(cidDir)
		}
		return nil, err
	}
	inR.Close()  // the child holds the read end now
	outW.Close() // ...and the write end; outR sees EOF when the child exits
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			// Kill and await the generation group before its cid cleanup. In tagged live binaries the
			// group leader is a resident gate wrapper, so a runtime cannot outlive this identity.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			inW.Close()
			outR.Close()
			waitACPProcessGroupGone(pid, acpCleanupTimeout)
			if cidPath != "" {
				if cid, rerr := os.ReadFile(cidPath); rerr == nil {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), acpCleanupTimeout)
					_ = a.rt.RemoveContainerContext(cleanupCtx, strings.TrimSpace(string(cid)))
					cancel()
				}
			}
			if cidDir != "" {
				os.RemoveAll(cidDir)
			}
		})
	}
	return &acpproxy.Child{In: inW, Out: outR, Stop: stop, Provider: provider, Account: account}, nil
}

func waitACPProcessGroupGone(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}
