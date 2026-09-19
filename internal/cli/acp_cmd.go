package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/liveprocess"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
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

// acpHost builds the real acpctl.Host: the rotation and models-cache policy the ACP control needs
// but cannot own itself (see internal/acpctl's Host doc). Called by cmdACP for production, and
// reused by this package's own tests so they construct a control with real behavior.
func acpHost() acpctl.Host {
	return acpctl.Host{
		ExpandLadder:     expandLadder,
		AccountsFor:      accountsFor,
		WriteModelsCache: writeModelsCache,
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
func (a *app) cmdACP(args []string) (int, error) {
	// The ACP proxy is ALWAYS in the path: it's coop's control point for the editor session —
	// restart resilience, plus rewriting the session so coop owns the toolbar (yolo, model default,
	// coop's plain/preset toolbar selectors). The OUTER process validates the args (fail fast), then
	// supervises; the INNER (COOP_ACP_INNER=1) runs the box.
	inner := args // the args the supervisor re-execs as `coop acp <inner>`; the inner re-parses them
	innerProcess := os.Getenv("COOP_ACP_INNER") != ""
	// coop's own network flags come off first, in BOTH processes: the outer admits
	// with them, and the inner strips them again so the leftover check below still
	// sees only the who slot. The inner never admits — its authority arrives from
	// the supervisor as a proved reference.
	args, err := a.takeNetworkFlags(args)
	if err != nil {
		return 2, err
	}
	// --bare serves a Q&A agent with no repository, context or tool, and needs none of what the
	// supervisor exists for (a project toolbar, provider switching, warm boxes), so it runs the
	// box directly below. --readonly is not offered here: an editor session mounts its repository
	// writable, and the read-only form that fronts a pinned fork is `coop fork <name> acp --readonly`.
	if args, err = a.takeExposureFlags("coop acp", args); err != nil {
		return 2, err
	}
	if a.mode == agents.ModeReadOnly {
		return 2, errors.New("coop acp does not take --readonly — front a fork read-only with 'coop fork <name> acp <target> --readonly', or investigate with 'coop <target> --readonly'")
	}
	peerVals, args, err := extractPeer("coop acp", args)
	if err != nil {
		return 2, err
	}
	if a.mode == agents.ModeBare && len(peerVals) > 0 {
		return 2, errors.New("a bare run consults no peers — drop --peer")
	}
	// Resolve the --peer peers HERE, before the outer/inner split — so an editor's
	// agent_servers entry with a bad peer (unknown/unauthed, or an @account) fails fast in the
	// OUTER process, not silently later inside the box.
	peers, err := a.resolvePeers("coop acp", peerVals)
	if err != nil {
		return 2, err
	}
	a.acpPeers = slices.Clone(peers)
	// The positional who-runs slot pins the session: a TARGET (provider[:model][/effort][@account],
	// so an editor's agent_servers entry runs ["acp","claude:opus@work"]) OR a PRESET NAME (routing +
	// role wiring; its lead is the agent). Parsed BEFORE the inner
	// env-override block so a preset-rotation rung (COOP_ACP_TARGET) still wins over the launch-time
	// model/account.
	model, profile, effort := "", "", ""
	tool, toolSet := "", false
	presetName := ""
	consumed := 0
	// takeWho classifies the positional who slot: a target folds its model/effort/account in and
	// sets the provider; a bare word is a preset name resolved below.
	takeWho := func(who string) error {
		if !isTargetHead(who) {
			presetName = who
			return nil
		}
		t, terr := agents.ParseTarget(who)
		if terr != nil {
			return terr
		}
		tool = t.Provider
		toolSet = true
		if terr := foldTarget(t, "coop acp", &model, &profile); terr != nil {
			return terr
		}
		effort = t.Effort
		return nil
	}
	if len(args) > 0 {
		if terr := takeWho(args[0]); terr != nil {
			return 2, terr
		}
		consumed = 1
	}
	// Reject leftover tokens rather than silently ignore them (loop/fork do the same) — the ACP
	// adapter takes no extra args, so `coop acp claude foo`/`--nope` is a mistake worth surfacing.
	if leftover := args[consumed:]; len(leftover) > 0 {
		return 2, ui.UnexpectedArgument(leftover[0], "coop acp", "coop acp [<target|preset>] [--peer <target>...]")
	}
	if a.mode == agents.ModeBare {
		if !toolSet {
			if presetName != "" {
				return 2, fmt.Errorf("a preset runs its roles from the box, which a bare run has none of — name its lead directly: coop acp <target> --bare")
			}
			return 2, noProviderErr("acp")
		}
		return a.acpBare(tool, model, profile, effort)
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
			model, effort, profile = t.Model, t.Effort, t.Account()
		}
		if os.Getenv(box.SessionNetworkCaptureEnv) != "" {
			bindings, err := applyACPAccountBindings(a.cfg, os.Getenv(acpAccountBindingsEnv))
			if err != nil {
				return 2, err
			}
			a.acpAccountBindings = bindings
		}
	}
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	tool = presetLeadAgent(p, tool, toolSet)
	if tool == "" {
		// The editor cannot select a provider until ACP has started. Keep this choice
		// automatic so the live toolbar and account recovery still own the selection.
		authed := box.AuthedAgents(a.cfg)
		if len(authed) == 0 {
			return 1, ui.CommandFailed("Could not start the editor agent", "No providers are signed in.",
				[2]string{"Sign in:", "coop login <agent>"},
				[2]string{"", "Then reconnect your editor."})
		}
		tool = authed[0]
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
	// The outer process owns the editor stream via the proxy; it builds coop's control layer (the
	// toolbar rewrite + preset/plain selectors) and re-execs `coop acp <inner>` (COOP_ACP_INNER
	// set) to run the box, the current selection carried in the env. The inner falls through to box.Run.
	if !innerProcess {
		repo, pj, err := loadProject(a.cfg.RepoOverride)
		if err != nil {
			return -1, err
		}
		ctrlModel := model
		if ctrlModel == "" {
			ctrlModel = a.cfg.ModelFor(tool)
		}
		ctrlEffort := effort
		if ctrlEffort == "" {
			ctrlEffort = a.cfg.EffortFor(tool)
		}
		// Admission happens ONCE, here, before any child: a toolbar provider switch
		// or a preset rung reuses this exact capture, so every provider this session
		// could spawn has to be in the scope its bundles derive from. Required
		// targets stay intact; optional toolbar choices contribute only complete
		// supported scopes. The control freezes that same scope after admission.
		initial := agents.Target{Provider: tool}
		if profile != "" {
			initial.Accounts = []string{profile}
		}
		scope, err := a.acpNetworkScope(repo, initial, peers, a.preset)
		if err != nil {
			return 1, err
		}
		// The capture belongs to the SUPERVISOR: it lives as long as the editor
		// session, and each child receives a reference to it, never authority.
		a.acpCapture, err = box.AdmitNetwork(a.cfg, a.rt, box.RunSpec{
			Repo: repo, Workdir: repo, Agent: tool, Peers: scope, Preset: a.preset, NetworkClient: egress.ClientACP,
			Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
		}, a.network.admission())
		if err != nil {
			return 1, err
		}
		if a.acpCapture != nil {
			a.acpNetworkTargets = slices.Clone(scope)
		}
		defer a.acpCapture.Close()
		// Ports the inner box will publish (.agent/project.yaml serve), reported to the editor once per
		// session. Deterministic host ports (project.HostPort), so these match what box.Run binds. Only
		// when egress is open — otherwise nothing publishes, so nothing to announce.
		var serveURLs []string
		if a.cfg.Egress == "open" {
			for _, port := range pj.Serve.Ports {
				serveURLs = append(serveURLs, fmt.Sprintf("box :%d → http://localhost:%d", port, project.HostPort(repo, port)))
			}
		}
		sel := acpctl.Selection{Account: profile, Preset: presetName}
		if toolSet {
			sel.Provider = tool
		}
		ctrl := acpctl.New(a.cfg, tool, ctrlModel, ctrlEffort, repo, sel, a.acpPresetNames(repo), serveURLs, acpHost())
		if a.acpCapture != nil {
			ctrl.LimitNetworkTargets(scope)
		}
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
	// A FILTERED child has no `docker run` to write one: its agent container is created by the
	// gateway engine, which records the exact id itself and refuses every unqualified runtime
	// argument. Its teardown is the cancellation below; after a kill, the next filtered launch
	// settles it (runBox).
	if cid := os.Getenv("COOP_ACP_CIDFILE"); cid != "" && os.Getenv(box.SessionNetworkCaptureEnv) == "" {
		extra = append(extra, "--cidfile", cid)
	}
	activityRepo, forkIdentity, err := forkspace.ResolveProjectBinding(repo)
	if err != nil {
		return 1, err
	}
	spec := box.RunSpec{
		// A supervisor (which reconnects the box) passes COOP_ACP_SUPERVISOR; that tags
		// the box so build/update can restart it and the supervisor can kill exactly it.
		Image: img, Repo: repo, Workdir: repo, Cmd: cmd, ForceNoTTY: true, Agent: tool, Serve: true, NetworkClient: egress.ClientACP,
		SupervisorID: os.Getenv("COOP_ACP_SUPERVISOR"), ShareACPSessions: true,
		ConsultLead: lead, Peers: peers, Preset: a.preset, Quiet: true,
		ExtraArgs:    extra,
		ActivityRepo: activityRepo, ActivityKind: forkspace.ExecutionACP,
		ActivityRole:   forkspace.ExecutionRole(os.Getenv("COOP_ACP_ACTIVITY_ROLE")),
		ActivitySource: os.Getenv("COOP_ACP_SUPERVISOR"),
		Homes:          a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	}
	if forkIdentity != nil {
		spec.ActivityKind = forkspace.ExecutionForkACP
		spec.ForkName = forkIdentity.Name
		spec.ForkGeneration = string(forkIdentity.Generation)
		spec.ForkOwner = forkctl.ForkContainerOwner(activityRepo, forkIdentity.Name, forkIdentity.Generation)
	}
	// This child's network authority arrives from the supervisor that started it,
	// as a reference it must prove against the owner-private store. A child
	// admits nothing: an approval that lands mid-session cannot widen it.
	capture, err := box.CapturedEgressFromEnvironment(a.cfg, spec)
	if err != nil {
		return 1, err
	}
	defer capture.Close()
	if capture != nil {
		if err := validateACPAccountBindings(a.cfg, spec, a.acpAccountBindings); err != nil {
			return 1, err
		}
		// Rebuild the concrete credential scope after the child applied its lead
		// target and preset. This is the final boundary before any provider home is
		// mounted, and catches role/default-account drift hidden by a portable sibling.
		if _, err := box.NetworkProviderBundles(a.cfg, spec); err != nil {
			return 1, fmt.Errorf("selected ACP scope is no longer available under this session's network rules: %w", err)
		}
		spec.CapturedEgress = capture
		// A filtered child owns a gateway, two volumes and a receipt. The
		// supervisor stops it with a signal, so that signal has to arrive as a
		// cancellation this run can clean up after.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		spec.Ctx = ctx
	}
	return a.runBox(spec)
}

// acpBare serves the target's ACP adapter under the bare profile: the shared base image, no
// repository, no project, no tool, over stdio, with no supervisor in between. The adapter takes
// no flags, so the no-tools switch is not on this command — it rides the session/new the ACP
// client sends (agents.Agent.ACPRestrictedSessionMeta), which is why only a client that knows
// the mode (the session daemon) can drive a bare session correctly; an editor entry runs the
// same box and gets a tool-less agent only if it sends that meta. The run label is the session
// daemon's cleanup receipt, exactly as for a fork's ACP child.
func (a *app) acpBare(tool, model, profile, effort string) (int, error) {
	if err := a.applyOneOff(tool, model, profile, effort); err != nil {
		return 2, err
	}
	img, code, err := a.restrictedImage()
	if err != nil {
		return code, err
	}
	if a.network.Mode != nil {
		a.cfg.SetEgress(string(*a.network.Mode))
	}
	// The run owns a host seed directory holding the credential projection; the signal the session
	// daemon ends a turn with has to arrive as a cancellation this run can clean up after.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Cmd: acpCommand(a.cfg, tool), ForceNoTTY: true, Agent: tool, Homes: a.cfg.Homes,
		Mode: agents.ModeBare, NetworkClient: egress.ClientACP, Quiet: true,
		RunID: sessionsvc.RunIDFromEnv(), Ctx: ctx,
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
	// `image inspect` fails the same way whether the image is missing or the daemon is gone, so
	// probe the daemon before blaming the image — otherwise a stopped Docker is reported as a
	// build the editor should wait through.
	if err := a.rt.EnsureDaemon(); err != nil {
		return ui.CommandFailed("Could not prepare the Coop box", "Docker is unavailable.",
			[2]string{"", "Start Docker, then reconnect your editor."})
	}
	ui.Heading("Checking the Coop box")
	ui.Note("  The box image is missing.")
	ui.Note("  Building it now…")
	if err := box.BuildWith(a.rt, a.cfg, repo, false, resolveVersion(), strings.NewReader(""), os.Stderr); err != nil {
		return ui.CommandFailed("Could not prepare the Coop box", err.Error(),
			[2]string{"", "Fix what the build reported above, then reconnect your editor."})
	}
	ui.Pass("Box ready")
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
	if st := a.acpResume; st != nil {
		ctrl.Restore(st.Ctrl)
		resume = &st.Proxy
		acpproxy.Trace("resumed from re-exec: %d session(s)", len(st.Proxy.Sessions))
		if st.PriorSupervisor != "" { // the previous generation's sweep failed — one retry, still before any box spawns
			if cerr := a.reapACPBoxes(st.PriorSupervisor); cerr != nil {
				ui.Warn("acp reload: previous generation's box cleanup failed again (%v) — a box labelled %s may linger until this supervisor exits", cerr, st.PriorSupervisor)
			}
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
	// (proxy replay only) instead of cold-booting one (measured 2026-09-19: ~0.8 s unrestricted, ~5.8 s
	// filtered). Behind the factory: a miss cold-spawns, so correctness is unaffected. COOP_ACP_WARM=0
	// opts out (a low-RAM escape hatch).
	warm := a.cfg.ACPWarm
	// The image a box starts from now. A warm box records it and is reused only while it still is — a
	// `coop build` mid-session must not leave a switch on the old one.
	currentImage := func() string {
		repo, err := box.ResolveRepo(a.cfg.RepoOverride)
		if err != nil {
			return ""
		}
		return a.rt.ImageID(box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride))
	}
	if warm && currentImage() == "" {
		// Without the image's identity no parked box can be proved current, so none would ever be
		// lent: keep nothing idle (Apple container, whose ids the runtime does not read, lands here).
		warm = false
		acpproxy.Trace("warm pool off: the box image's id cannot be read")
	}
	// Stopping cancels warm fills still in flight: one that has not launched launches nothing, so the
	// pool's reap does not wait for a box nobody will use.
	warmFills, cancelWarmFills := context.WithCancel(context.Background())
	pool := acpctl.NewWarmPool(warm, func(provider string) (*acpproxy.Child, error) {
		image := currentImage()
		// On the account the selector's Auto would pick, so the switch it serves can match it.
		target := ctrl.ResolveNetworkTarget(agents.Target{Provider: provider})
		child, err := a.spawnBox(warmFills, self, inner, superID, ctrl, target, "", true, os.Stderr, forkspace.ExecutionRoleWarm)
		if child != nil {
			child.Image = image
		}
		return child, err
	})
	// After any spawn — Run's first one included, which fans the pool out in the background, so startup
	// latency is unchanged — the pool re-centres on the provider now in use: warm the others, the one
	// just left included, and drop a spare of the active one.
	rebalance := func(active string) {
		go func() { pool.Rebalance(active, ctrl.SpawnableProviders(active)) }()
	}
	// Warm the others while the lead's own box starts, as before any switch: the first spawn's
	// rebalance repeats it (a fill already held or in flight is skipped), and a lead that waits out a
	// reset, or fails, still leaves the providers it could switch to ready.
	rebalance(ctrl.LeadProvider())
	factory := func(ctx context.Context) (*acpproxy.Child, error) {
		t, psName, ok := ctrl.SpawnTarget()
		if err := ctrl.ValidateNetworkTarget(t, psName); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err // a superseded spawn launches nothing — nor takes a parked box it would leak
		}
		if acpctl.WarmSwitch(t, psName, ok, a.cfg.ModelFor(t.Provider), a.cfg.EffortFor(t.Provider)) && a.warmCheckoutProven(ctrl, t, psName) {
			if c := pool.Checkout(t.Provider, t.Account(), currentImage()); c != nil {
				acpproxy.Trace("spawn: warm box for %s@%s", c.Provider, c.Account)
				rebalance(c.Provider)
				return c, nil
			}
		}
		child, cerr := a.spawnBox(ctx, self, inner, superID, ctrl, t, psName, ok, os.Stderr, forkspace.ExecutionRoleActive)
		if cerr != nil {
			return nil, cerr
		}
		acpproxy.Trace("spawn: cold box for %s@%s", child.Provider, child.Account)
		rebalance(child.Provider)
		return child, nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	reapPool := func() { cancelWarmFills(); pool.Reap() }
	defer reapPool() // Stop held warm boxes on any exit path; the label sweep still reaps their containers
	// Thread bindings outlive this process: a reopened thread's session/load must reach the provider and
	// native session the conversation actually continued on, not the transcript stub its editor id names.
	bindings := acpctl.OpenThreadBindings(acpctl.ThreadBindingsDir(a.cfg))
	// The parked boxes are independent of the active one, so they stop while it does; the deferred
	// Reap then waits for that same teardown to finish.
	stopping := func() { go reapPool() }
	err = acpproxy.RunWith(ctx, os.Stdin, os.Stdout, factory, ctrl.Hooks(), acpproxy.RunOpts{Resume: resume, Reload: reload, Bindings: bindings, Stopping: stopping})
	// A SIGHUP reload: write the combined state to a 0600 temp file and re-exec THIS binary in place —
	// same PID + fd 0/1/2, so the editor's transport never breaks. Run's reload path already stopped
	// the box; reap the warm boxes here (execve replaces the image, so deferred reap won't run) and
	// skip the label sweep — the re-exec'd process regenerates its own superID and owns the next box.
	if snap, ok := acpproxy.ReloadSnapshot(err); ok {
		reapPool()
		// Sweep any box still labelled with THIS superID before exec — a warm spawn that was mid-flight
		// (reap only stops boxes already parked) would otherwise reparent to init and never be reaped
		// (the re-exec'd process uses a fresh superID). Safe here: Run already stopped the active box
		// and no new box is spawned until after exec, so nothing we need is swept.
		warning, prior := acpReloadAfterCleanup(superID, a.reapACPBoxes(superID))
		if warning != "" {
			ui.Warn("%s", warning)
		}
		path, werr := acpctl.WriteResumeState(acpctl.ResumeState{Proxy: *snap, Ctrl: ctrl.Snapshot(), PriorSupervisor: prior})
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
	cleanupErr := a.reapACPBoxes(superID)
	if err != nil && !errors.Is(err, context.Canceled) {
		return 1, errors.Join(err, cleanupErr)
	}
	return 0, cleanupErr
}

// acpReloadAfterCleanup decides what a failed pre-exec box sweep means for a SIGHUP reload: the
// reload goes ahead. `coop update` and `coop build` SIGHUP every supervisor at once, so one
// transient `docker ps` / `rm -f` failure must not end editor sessions; Run already stopped the
// active box, and anything the sweep missed still carries this supervisor's label, so the next
// generation retries the sweep (PriorSupervisor) and the orphan sweep reaps it once the supervisor
// is gone. Only the reload path is this lenient — the final teardown sweep returns its error.
func acpReloadAfterCleanup(superID string, sweepErr error) (warning, priorSupervisor string) {
	if sweepErr == nil {
		return "", ""
	}
	return fmt.Sprintf("acp reload: box cleanup failed (%v) — reloading anyway; the next generation retries the sweep", sweepErr), superID
}

func (a *app) reapACPBoxes(superID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), acpCleanupTimeout)
	defer cancel()
	if _, err := a.rt.RemoveByLabel(ctx, box.LabelSupervisor, superID); err != nil {
		return err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return err
	}
	authorityRepo, _, err := forkspace.ResolveProjectBinding(repo)
	if err != nil {
		return err
	}
	return forkspace.RemoveDeadExecutionsBySource(authorityRepo, superID)
}

// acpChildCapture renders this supervisor's frozen network authority as the
// reference ONE child launch proves. Each child gets its own attempt identity,
// so `coop net runs` relates a run to the child that produced it; the session
// identity is the supervisor's, which is what the editor session is.
func (a *app) acpChildCapture(superID string) (string, error) {
	if a.acpCapture == nil {
		return "", nil
	}
	attempt, err := newSupervisorID()
	if err != nil {
		return "", err
	}
	return box.SessionNetworkCapture{
		Project: a.acpCapture.Project, Fingerprint: a.acpCapture.Fingerprint,
		Qualification: a.acpCapture.QualificationID,
		SessionID:     "acp-" + superID, AttemptID: "acp-" + attempt,
	}.Encode()
}

func newSupervisorID() (string, error) {
	idbuf := make([]byte, 8)
	if _, err := rand.Read(idbuf); err != nil {
		return "", err
	}
	return hex.EncodeToString(idbuf), nil
}

const acpCleanupTimeout = 5 * time.Second

// acpFilteredStopGrace bounds a filtered child's own teardown — agent absence, the gateway's final
// observation, then its guard, controller and volumes — before its process group is killed. It takes
// under two seconds. A single runtime call may take up to its own minute (box.filteredControlTimeout),
// so a wedged daemon is killed mid-teardown here; the next filtered launch's recovery settles the rest.
const acpFilteredStopGrace = 30 * time.Second
const acpAccountBindingsEnv = "COOP_ACP_ACCOUNTS"

type acpAccountBinding struct {
	Account string `json:"account"`
	Default string `json:"default"`
}

// applyACPAccountBindings restores the supervisor's exact provider/account
// choices before the child builds credential mounts. The host parent is the
// sole writer of this scrubbed variable; a captured child missing it refuses.
func applyACPAccountBindings(cfg *config.Config, raw string) (map[string]acpAccountBinding, error) {
	if raw == "" {
		return nil, errors.New("filtered ACP account bindings are missing")
	}
	var bindings map[string]acpAccountBinding
	if err := json.Unmarshal([]byte(raw), &bindings); err != nil || len(bindings) == 0 {
		return nil, errors.New("filtered ACP account bindings are malformed")
	}
	for provider, binding := range bindings {
		target, err := agents.ParseTarget(provider + "@" + binding.Account)
		if err != nil || target.Provider != provider || target.Account() != binding.Account || binding.Default == "" {
			return nil, errors.New("filtered ACP account bindings are malformed")
		}
		if current := cfg.DefaultProfileOf(provider); current != binding.Default {
			return nil, fmt.Errorf("%s default account changed from %q to %q after this filtered ACP session started", provider, binding.Default, current)
		}
		cfg.SetActiveProfile(provider, binding.Account)
	}
	return bindings, nil
}

// validateACPAccountBindings closes the tiny parent-load/child-load race: a
// preset edited to add a role after the supervisor's final check cannot fall
// back to that provider's current default. Every provider the child can mount
// must name exactly the account the parent handed it.
func validateACPAccountBindings(cfg *config.Config, spec box.RunSpec, bindings map[string]acpAccountBinding) error {
	need := map[string]string{}
	add := func(provider string) error {
		if provider == "" {
			return nil
		}
		account := cfg.ActiveProfile(provider)
		bound, ok := bindings[provider]
		if !ok || bound.Account != account || bound.Default != cfg.DefaultProfileOf(provider) {
			return fmt.Errorf("filtered ACP has no supervisor-approved binding for %s account %q", provider, account)
		}
		need[provider] = account
		return nil
	}
	if err := add(spec.Agent); err != nil {
		return err
	}
	for _, peer := range spec.Peers {
		if err := add(peer.Provider); err != nil {
			return err
		}
	}
	if spec.Preset != nil {
		for _, provider := range spec.Preset.RunnableRoleAgents(spec.Agent) {
			if err := add(provider); err != nil {
				return err
			}
		}
	}
	if len(need) == 0 {
		return errors.New("filtered ACP has no supervisor-approved credential scope")
	}
	return nil
}

// acpFilteredSpawnScope returns both the complete preset closure that must
// still be qualified after a reset wait and the exact account each provider in
// this child will mount. The latter is passed to the re-exec so a changed host
// default cannot widen the frozen supervisor session.
func (a *app) acpFilteredSpawnScope(lead agents.Target, presetName string) ([]agents.Target, map[string]acpAccountBinding, error) {
	if lead.Provider == "" {
		return nil, nil, errors.New("filtered ACP needs a concrete provider")
	}
	if lead.Account() == "" {
		lead.Accounts = []string{a.cfg.ActiveProfile(lead.Provider)}
	}
	var targets []agents.Target
	bindings := map[string]acpAccountBinding{}
	addTarget := func(target agents.Target) {
		if target.Account() == "" {
			target.Accounts = []string{a.cfg.ActiveProfile(target.Provider)}
		}
		targets = append(targets, target)
	}
	bind := func(provider, account string) error {
		binding := acpAccountBinding{Account: account, Default: a.cfg.DefaultProfileOf(provider)}
		if existing, ok := bindings[provider]; ok && existing != binding {
			return fmt.Errorf("filtered ACP selected two %s accounts (%q and %q) for one child", provider, existing.Account, account)
		}
		bindings[provider] = binding
		return nil
	}
	addTarget(lead)
	if err := bind(lead.Provider, lead.Account()); err != nil {
		return nil, nil, err
	}
	for _, peer := range a.acpPeers {
		if peer.Account() == "" {
			peer.Accounts = []string{a.cfg.ActiveProfile(peer.Provider)}
		}
		addTarget(peer)
		if err := bind(peer.Provider, peer.Account()); err != nil {
			return nil, nil, err
		}
	}
	if presetName == "" {
		return targets, bindings, nil
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return nil, nil, err
	}
	p, err := preset.Load(repo, a.cfg.GlobalPresetsDir(), presetName)
	if err != nil {
		return nil, nil, err
	}
	closure, err := a.acpPresetNetworkTargets(p)
	if err != nil {
		return nil, nil, err
	}
	targets = append(targets, closure...)
	for _, provider := range p.RunnableRoleAgents(lead.Provider) {
		account := a.cfg.ActiveProfile(provider)
		if provider == lead.Provider {
			account = lead.Account()
		}
		if err := bind(provider, account); err != nil {
			return nil, nil, err
		}
	}
	return targets, bindings, nil
}

func cleanACPChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		// The network capture is minted per child by this supervisor. An inherited
		// one names a snapshot this session never admitted, so it never rides in.
		case "COOP_ACP_INNER", "COOP_ACP_SUPERVISOR", "COOP_ACP_TARGET", "COOP_ACP_PRESET", "COOP_ACP_CIDFILE", "COOP_ACP_RESUME_STATE", "COOP_ACP_ACTIVITY_ROLE", acpAccountBindingsEnv,
			box.SessionNetworkCaptureEnv,
			liveprocess.ControlFDEnv, liveprocess.ProcessDirEnv, liveprocess.CleanupIDEnv, liveprocess.RevokePathEnv:
			continue
		}
		out = append(out, item)
	}
	return out
}

// warmCheckoutProven reports whether a parked box may serve t: in a filtered session only after t passes
// the same launch proof a cold box would.
func (a *app) warmCheckoutProven(ctrl *acpctl.Control, t agents.Target, psName string) bool {
	if a.acpCapture == nil {
		return true
	}
	_, err := a.acpFilteredLaunchProof(ctrl, t, psName)
	return err == nil
}

// acpFilteredLaunchProof re-proves a filtered target immediately before a box serves it — a cold launch
// after any reset wait, and a warm box at checkout, since the conditions it was parked under can change —
// and returns the child's account bindings.
func (a *app) acpFilteredLaunchProof(ctrl *acpctl.Control, t agents.Target, psName string) (map[string]acpAccountBinding, error) {
	if ctrl != nil {
		if err := ctrl.ValidateNetworkTarget(t, psName); err != nil {
			return nil, err
		}
	}
	targets, bindings, err := a.acpFilteredSpawnScope(t, psName)
	if err == nil {
		for _, target := range targets {
			if !slices.ContainsFunc(a.acpNetworkTargets, func(admitted agents.Target) bool {
				return admitted.Provider == target.Provider && admitted.Account() == target.Account()
			}) {
				err = fmt.Errorf("%s account %q was not admitted by this filtered ACP supervisor", target.Provider, target.Account())
			}
			if ctrl != nil {
				if validateErr := ctrl.ValidateNetworkTarget(target, ""); err == nil {
					err = validateErr
				}
			}
			if err == nil {
				_, err = box.NetworkTargetBundle(a.cfg, target, egress.ClientACP)
			}
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("selected ACP target is no longer available under this session's network rules: %w", err)
	}
	return bindings, nil
}

// spawnBox execs a `coop acp` inner box for the given spawn target and wraps it as an acpproxy.Child
// — the ONE spawn path for the live factory, warm-pool prewarm, and short-lived model probe, so each
// gets the same credentials, process isolation, and teardown.
func (a *app) spawnBox(ctx context.Context, self string, inner []string, superID string, ctrl *acpctl.Control, t agents.Target, psName string, hasTarget bool, stderr io.Writer, activityRoles ...forkspace.ExecutionRole) (*acpproxy.Child, error) {
	provider := t.Provider
	if provider == "" && ctrl != nil {
		provider = ctrl.LeadProvider()
	}
	if ctrl != nil {
		if a.acpCapture != nil {
			t = ctrl.ResolveNetworkTarget(t)
		}
		if err := ctrl.ValidateNetworkTarget(t, psName); err != nil {
			return nil, err
		}
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
	activityRole := forkspace.ExecutionRoleActive
	if len(activityRoles) > 0 && activityRoles[0] != "" {
		activityRole = activityRoles[0]
	}
	env := append(cleanACPChildEnv(os.Environ()), "COOP_ACP_INNER=1", "COOP_ACP_SUPERVISOR="+superID,
		"COOP_ACP_ACTIVITY_ROLE="+string(activityRole))
	capture, err := a.acpChildCapture(superID)
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	if capture != "" {
		env = append(env, box.SessionNetworkCaptureEnv+"="+capture)
	}
	if hasTarget {
		if ctrl != nil { // model probes use a bare provider target and need no reset/preset wait
			switch acct := t.Account(); {
			case psName != "":
				ctrl.WaitForPresetRung(ctx)
			case acct != "" && activityRole == forkspace.ExecutionRoleWarm && ctrl.Cooling(t.Provider, acct):
				// A warm box waits for nothing: the pool leaves the slot empty instead.
				inR.Close()
				inW.Close()
				outR.Close()
				outW.Close()
				return nil, fmt.Errorf("%s account %q is waiting out a rate limit", t.Provider, acct)
			case acct != "":
				ctrl.WaitForReset(ctx, t.Provider, acct)
			}
		}
		if err := ctx.Err(); err != nil {
			inR.Close()
			inW.Close()
			outR.Close()
			outW.Close()
			return nil, err
		}
		if ctrl != nil {
			// Presence is the selection signal; an empty value explicitly clears a positional
			// launch preset in the child instead of letting the original argv resurrect it.
			env = append(env, "COOP_ACP_PRESET="+psName)
		}
		env = append(env, "COOP_ACP_TARGET="+t.String())
		acpproxy.Trace("spawn box on target=%s preset=%s", t.String(), psName)
	}
	// Reset waits can last for hours. Re-prove the complete preset and exact
	// authentication families after the wait and immediately before launching
	// the child; open/offline ACP deliberately retains every native auth mode.
	if a.acpCapture != nil {
		bindings, err := a.acpFilteredLaunchProof(ctrl, t, psName)
		if err != nil {
			inR.Close()
			inW.Close()
			outR.Close()
			outW.Close()
			return nil, err
		}
		encoded, err := json.Marshal(bindings)
		if err != nil {
			inR.Close()
			inW.Close()
			outR.Close()
			outW.Close()
			return nil, fmt.Errorf("encode ACP account bindings: %w", err)
		}
		env = append(env, acpAccountBindingsEnv+"="+string(encoded))
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
	activityRepo := ""
	if currentRepo, resolveErr := box.ResolveRepo(a.cfg.RepoOverride); resolveErr == nil {
		if canonical, _, bindingErr := forkspace.ResolveProjectBinding(currentRepo); bindingErr == nil {
			activityRepo = canonical
		}
	}
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			// End and await the generation group before its cid cleanup. In tagged live binaries the
			// group leader is a resident gate wrapper, so a runtime cannot outlive this identity.
			grace := time.Duration(0)
			if a.acpCapture != nil {
				grace = acpFilteredStopGrace
			}
			stopACPChild(pid, grace)
			inW.Close()
			outR.Close()
			groupGone := waitACPProcessGroupGone(pid, acpCleanupTimeout)
			runtimeGone := false
			if cidPath != "" {
				if cid, rerr := os.ReadFile(cidPath); rerr == nil {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), acpCleanupTimeout)
					runtimeGone = a.rt.RemoveContainerContext(cleanupCtx, strings.TrimSpace(string(cid))) == nil
					cancel()
				}
			}
			// Apple's container CLI has no cidfile support. The execution record is published before
			// runtime launch and its id is also a unique container label, so after the supervising
			// process is gone it is the exact fallback cleanup authority. Never use the shared ACP
			// supervisor label here: a provider swap may already have published its replacement box.
			if groupGone && !runtimeGone && activityRepo != "" {
				runtimeGone = a.reapACPChildBoxes(activityRepo, superID, pid)
			}
			if groupGone && runtimeGone && activityRepo != "" {
				_ = forkspace.RemoveDeadExecutionsBySource(activityRepo, superID)
			}
			if cidDir != "" {
				os.RemoveAll(cidDir)
			}
		})
	}
	setActive := func(active bool) {
		if activityRepo == "" {
			return
		}
		role := forkspace.ExecutionRoleWarm
		if active {
			role = forkspace.ExecutionRoleActive
		}
		_ = forkspace.UpdateExecutionRoleByPID(activityRepo, pid, role)
	}
	return &acpproxy.Child{In: inW, Out: outR, Stop: stop, SetActive: setActive, Provider: provider, Account: account}, nil
}

// reapACPChildBoxes removes only containers carrying the execution IDs published by one stopped
// inner ACP process. An empty, readable set is proof that the child never reached runtime launch or
// already completed cleanup; an unreadable registry remains cleanup-pending for the final sweep.
func (a *app) reapACPChildBoxes(repo, superID string, pid int) bool {
	observations, problems := forkspace.Executions(repo)
	if len(problems) > 0 {
		return false
	}
	for _, observation := range observations {
		if observation.Record.PID != pid || observation.Record.SourceID != superID {
			continue
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), acpCleanupTimeout)
		_, err := a.rt.RemoveByLabel(cleanupCtx, box.LabelExecution, observation.Record.ID)
		cancel()
		if err != nil {
			return false
		}
	}
	return true
}

// stopACPChild ends an inner ACP child's process group. A filtered child owns a gateway, two volumes and
// a receipt, and tears them down itself when its run is cancelled, so it gets SIGTERM and up to grace to
// finish; SIGKILL first would strand all of it. Any child still running after that — or any child when
// grace is zero, which has nothing of its own to tear down — is killed.
func stopACPChild(pgid int, grace time.Duration) {
	if grace > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		if waitACPProcessGroupGone(pgid, grace) {
			return
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
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
