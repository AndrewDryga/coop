package cli

import (
	"slices"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/project"
)

// presetSkip is a preset the editor's selector leaves out because it does not load.
type presetSkip struct {
	name string
	err  error
}

// editorServePorts lists the .agent/project.yaml serve ports an editor session's boxes publish, for
// each new thread's start message. Host ports are deterministic (project.HostPort): the same
// numbers box.Run publishes, whenever the host port is still free. Open and filtered boxes both
// publish (a filtered one on its gateway); a box with networking off publishes nothing.
func editorServePorts(mode egress.Mode, repo string, ports []int) []acpctl.ServePort {
	if mode != egress.Open && mode != egress.Filtered {
		return nil
	}
	var serve []acpctl.ServePort
	for _, port := range ports {
		serve = append(serve, acpctl.ServePort{Box: port, Host: project.HostPort(repo, port)})
	}
	return serve
}

// editorServicesWait bounds how long a finishing coop acp waits for its project-services lookup,
// which runs docker compose without a deadline of its own.
const editorServicesWait = 5 * time.Second

// startEditorNotice fills in what a new thread's start message says beyond its ports: a filtered
// session's limits and the problems found now. Both read local state only (a sign-in record waits
// at most a second on its lock), so they are set before the editor gets its first answer.
func (a *app) startEditorNotice(ctrl *acpctl.Control, skipped []presetSkip) {
	setEditorNetwork(ctrl, a.acpCapture, a.acpBrokeredMCP)
	ctrl.AddStartProblems(editorStartProblems(a.cfg, skipped, time.Now())...)
}

// setEditorNetwork puts what capture's boxes may reach into every later thread's start message; a
// session's network refresh calls it again once a box under newly approved rules is live. Open and
// offline sessions pass no capture and say nothing about the network.
func setEditorNetwork(ctrl *acpctl.Control, capture *box.CapturedEgress, brokeredMCP []string) {
	if capture == nil {
		return
	}
	if limit, err := editorNetworkLimit(capture, brokeredMCP); err == nil {
		ctrl.SetNetwork(limit)
	}
}

// listEditorServices lists, in the background, the project services this session's boxes start, at
// the host ports the session's own Compose project publishes (it is owned by superID, so its ports
// differ from `coop up`'s), and returns the call that waits for it. Listing asks the container
// runtime, so a thread that opens first gets the services in the next one; the session waits for
// the lookup, briefly, before it returns, so nothing it starts outlives the command.
func (a *app) listEditorServices(ctrl *acpctl.Control, repo, superID string) (wait func()) {
	pj, err := project.Load(repo)
	if err != nil {
		return func() {}
	}
	cf := box.ComposeFileAt(repo, pj.ComposeRel())
	if cf == "" || !box.EditorStartsServices(a.cfg, a.rt, pj) {
		return func() {}
	}
	cfg, rt, done := a.cfg.Clone(), a.rt, make(chan struct{}) // the supervisor keeps using a.cfg
	go func() {
		defer close(done)
		var links []acpctl.ServiceLink
		for _, port := range box.SessionServicePorts(rt, repo, cf, superID, append(box.ConfigExposureRoots(cfg), repo)...) {
			links = append(links, acpctl.ServiceLink{Name: port.Service, Scheme: port.Scheme, Host: port.HostPort})
		}
		ctrl.SetServices(links)
	}()
	return func() {
		select {
		case <-done:
		case <-time.After(editorServicesWait):
		}
	}
}

// editorNetworkLimit is what a filtered session's boxes may reach, read from the rules its capture
// froze, which every box of the session enforces; a site two rules reach is named once. MCP servers
// the credential broker reaches for a box hold no grant there, so brokeredMCP names them, as the
// launch line does.
func editorNetworkLimit(capture *box.CapturedEgress, brokeredMCP []string) (acpctl.NetworkLimit, error) {
	policy, err := capture.Policy()
	if err != nil {
		return acpctl.NetworkLimit{}, err
	}
	return networkLimitOf(policy, brokeredMCP), nil
}

// networkLimitOf groups policy's grants, with the brokered MCP servers beside them.
func networkLimitOf(policy egress.Snapshot, brokeredMCP []string) acpctl.NetworkLimit {
	allowed := box.NetworkAllowedBy(policy, nil, brokeredMCP...)
	var sites []string
	for _, site := range append(allowed.Approved, allowed.ThisRun...) {
		if !slices.Contains(sites, site) {
			sites = append(sites, site)
		}
	}
	return acpctl.NetworkLimit{Sites: sites, Servers: allowed.Servers, Other: allowed.Other}
}

// editorStartProblems lists what an editor session finds wrong before its first thread opens:
// saved sign-ins that can no longer work, which only leave a provider or account missing from the
// toolbar without a word, and presets the selector left out.
func editorStartProblems(cfg *config.Config, skipped []presetSkip, now time.Time) []acpctl.Problem {
	var problems []acpctl.Problem
	for _, provider := range agents.Names() {
		problems = append(problems, signInProblems(cfg, provider, now)...)
	}
	for _, s := range skipped {
		// The listing shows why each preset failed; naming one would not do (`coop presets init`
		// creates a preset).
		problems = append(problems, acpctl.Problem{Title: `The "` + s.name + `" preset is missing from the toolbar.`,
			Detail: "It could not be loaded.", Command: "coop presets", Purpose: "on the host, to see why"})
	}
	return problems
}

// signInProblems names provider's saved sign-ins that can no longer work, judged by
// box.ProfileSignIn, the answer coop credentials, the model menu and a launch all give. A provider
// with no working account left is named alone; otherwise each dead account is.
func signInProblems(cfg *config.Config, provider string, now time.Time) []acpctl.Problem {
	ag, ok := agents.Get(provider)
	if !ok {
		return nil
	}
	accounts := []string{cfg.DefaultProfileOf(provider)}
	profiles, _ := box.EffectiveProfiles(cfg, provider)
	for _, account := range profiles {
		if account != accounts[0] {
			accounts = append(accounts, account)
		}
	}
	var dead []string
	working := false
	for _, account := range accounts {
		switch box.ProfileSignIn(cfg, provider, account, now) {
		case agents.SignInAgain:
			dead = append(dead, account)
		case agents.SignedIn:
			working = true
		}
	}
	var problems []acpctl.Problem
	for _, account := range dead {
		title := ag.DisplayName() + " isn't available."
		if working || len(dead) > 1 {
			title = ag.DisplayName() + ` account "` + account + `" isn't available.`
		}
		problems = append(problems, acpctl.Problem{Title: title, Detail: "Its saved sign-in no longer works.",
			Command: agents.LoginCommand(provider + "@" + account), Purpose: "on the host"})
	}
	return problems
}
