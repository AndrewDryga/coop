package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

// superviseForkACP gives a local fork the same ACP permission and target-setting
// handling as a normal editor session without exposing provider or preset switches:
// this command fixes the fork, provider and account, while native model/effort
// choices remain available. A fork made from a preset runs that preset as `coop acp
// <preset>` does, its ladder, roles and limit rotation included, and stays on it. Its
// child still takes the forkACP path, which owns the parent's image, fork generation and
// network policy. scope is what admission covered for a preset.
func (a *app) superviseForkACP(repo, workspace, name string, target agents.Target, presetName string, peers, scope []agents.Target, capture *box.CapturedEgress) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}
	superID := newSupervisorID()
	a.acpCapture = capture
	a.acpPeers = peers
	inner := []string{"fork", name, "acp"}
	var ctrl *acpctl.Control
	var hooks *acpproxy.Hooks
	if presetName != "" {
		// Each child's rung and preset ride in its environment (spawnBox), so its argv names none. The
		// control reads the preset from the parent, as the child does (loadRunPreset).
		ctrl = acpctl.New(a.cfg, target.Provider, a.cfg.ModelFor(target.Provider), a.cfg.EffortFor(target.Provider), repo,
			acpctl.Selection{Preset: presetName}, []string{presetName}, nil, acpHost())
		if capture != nil {
			a.acpNetworkTargets = slices.Clone(scope)
			ctrl.LimitNetworkTargets(scope)
		}
		hooks = presetForkACPHooks(ctrl, repo, workspace, name, presetName)
	} else {
		if account := a.cfg.ActiveProfile(target.Provider); account != "" {
			target.Accounts = []string{account}
		}
		if capture != nil {
			a.acpNetworkTargets, _, err = a.acpFilteredSpawnScope(target, "")
			if err != nil {
				return 1, err
			}
		}
		inner = append(inner, target.String())
		ctrl = newForkACPControl(a.cfg, target, workspace)
		hooks = fixedForkACPHooks(ctrl, repo, workspace)
	}
	for _, peer := range peers {
		inner = append(inner, "--peer", peer.String())
	}
	networkArgs, err := a.network.args()
	if err != nil {
		return 2, err
	}
	inner = append(inner, networkArgs...)
	if a.acpSupervise != nil {
		return a.acpSupervise(inner, ctrl)
	}
	// A preset child starts on the rung the control names now: a limit moves it along the ladder.
	spawnTarget := func() (agents.Target, string, bool, *acpctl.Control) {
		if presetName == "" {
			return target, "", false, nil
		}
		t, ps, ok := ctrl.SpawnTarget()
		return t, ps, ok, ctrl
	}
	// Build readiness must use the same fork-local approval as its child admission.
	restarter := &acpproxy.Restarter{}
	a.acpBoxWait = a.newACPBoxWait(ctrl, restarter, box.RunSpec{Repo: workspace, Workdir: workspace, Agent: target.Provider, Network: a.cfg.Network, SupervisorID: superID}, func() {}, func() {})
	defer a.acpBoxWait.close()
	if err := a.acpBoxWait.refresh(context.Background()); err != nil {
		acpproxy.Trace("box wait: could not look: %v", err) // proves nothing: the first child's own proofs decide
	}
	a.sweepBeforeFirstBox(workspace) // the fork's own children run their boxes and services in the fork
	factory := func(ctx context.Context) (*acpproxy.Child, error) {
		if err := a.acpBoxWait.refreshStale(ctx); err != nil {
			acpproxy.Trace("box wait: could not look: %v", err)
		}
		t, ps, ok, spawnCtrl := spawnTarget()
		child, err := a.spawnBox(ctx, self, inner, superID, spawnCtrl, t, ps, ok, os.Stderr)
		if err == nil {
			a.acpBoxWait.started(child)
		}
		return child, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	a.acpBoxWait.start()
	hooks.HeldCommand = a.acpBoxWait.heldCommand
	err = acpproxy.RunWith(ctx, os.Stdin, os.Stdout, factory, hooks, acpproxy.RunOpts{NoEditorHostAccess: true, RestrictedNetwork: a.acpRestrictedNetwork(), Restarter: restarter, Stopping: func() { go a.acpBoxWait.close() }, ChildFailed: a.acpBoxWait.childFailedHook()})
	a.acpBoxWait.close()
	cleanupErr := a.reapACPBoxes(superID)
	if cleanupErr == nil {
		a.removeACPServices(workspace, superID) // the fork's own children ran its services in the fork
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return 1, errors.Join(err, cleanupErr)
	}
	return 0, cleanupErr
}

func newForkACPControl(cfg *config.Config, target agents.Target, workspace string) *acpctl.Control {
	model := target.Model
	if model == "" {
		model = cfg.ModelFor(target.Provider)
	}
	effort := target.Effort
	if effort == "" {
		effort = cfg.EffortFor(target.Provider)
	}
	return acpctl.New(cfg, target.Provider, model, effort, workspace,
		acpctl.Selection{}, nil, nil, acpHost())
}

func fixedForkACPHooks(ctrl *acpctl.Control, repo, workspace string) *acpproxy.Hooks {
	base := ctrl.Hooks()
	return &acpproxy.Hooks{
		SessionReady:     base.SessionReady,
		InjectedResponse: base.InjectedResponse,
		AutoReply:        base.AutoReply,
		ChildReset:       base.ChildReset,
		PromptHeld:       base.PromptHeld,
		ToEditor: func(line []byte) ([]byte, bool) {
			ctrl.ObserveNativeTargetResponse(line)
			return line, false
		},
		FromEditor: func(line []byte) (bool, []byte, []byte, bool) {
			handled, response, rewritten, restart := forkACPFromEditor(line, repo, workspace)
			if !handled {
				ctrl.ObserveNativeTargetRequest(line)
			}
			return handled, response, rewritten, restart
		},
	}
}

// presetForkACPHooks are coop acp's hooks for the fork's preset session, which rotate the lead
// ladder on a limit and show the preset with its live rung. The fork stays on its preset: the
// editor's choice of another preset, of none, or of a plain provider or account is refused.
func presetForkACPHooks(ctrl *acpctl.Control, repo, workspace, name, presetName string) *acpproxy.Hooks {
	hooks := ctrl.Hooks()
	fromEditor := hooks.FromEditor
	hooks.FromEditor = func(line []byte) (bool, []byte, []byte, bool) {
		handled, response, rewritten, restart := forkACPFromEditor(line, repo, workspace)
		if handled {
			return handled, response, rewritten, restart
		}
		if rewritten != nil {
			line = rewritten
		}
		if id, refused := forkPresetSwitch(line, presetName); refused {
			return true, forkPresetSwitchRefused(id, name, presetName), nil, false
		}
		handled, response, toAdapter, restart := fromEditor(line)
		if !handled && toAdapter == nil {
			toAdapter = rewritten
		}
		return handled, response, toAdapter, restart
	}
	return hooks
}

// forkPresetSwitch reports whether line asks to change Coop's own preset, provider or account
// selection to anything but presetName, with the request's id.
func forkPresetSwitch(line []byte, presetName string) (json.RawMessage, bool) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ConfigID string `json:"configId"`
			Value    string `json:"value"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &request) != nil || request.Method != "session/set_config_option" {
		return nil, false
	}
	switch request.Params.ConfigID {
	case acpctl.CoopPresetID:
		return request.ID, request.Params.Value != presetName
	case acpctl.CoopProviderID, acpctl.CoopAccountID:
		return request.ID, true
	}
	return nil, false
}

func forkPresetSwitchRefused(id json.RawMessage, name, presetName string) []byte {
	message := fmt.Sprintf("Fork %s runs preset %s here. To run something else, open it with: coop fork %s acp <target>", name, presetName, name)
	response, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32602, "message": message},
	})
	return append(response, '\n')
}

// forkACPPresetScope is every target a preset fork's editor session may start, for its one
// admission: each rung of the preset's lead ladder with the peers beside it, and the preset's roles.
func (a *app) forkACPPresetScope(presetName string, peers []agents.Target) ([]agents.Target, error) {
	leads, err := expandLadder(a.cfg, a.preset.Lead().Provider, a.preset.LeadTargets)
	if err != nil {
		return nil, err
	}
	a.acpPeers = peers
	var scope []agents.Target
	seen := map[string]bool{}
	for _, lead := range leads {
		targets, _, err := a.acpFilteredSpawnScope(lead, presetName)
		if err != nil {
			return nil, err
		}
		for _, target := range targets {
			if !seen[target.String()] {
				seen[target.String()] = true
				scope = append(scope, target)
			}
		}
	}
	return scope, nil
}

// The editor starts Coop from the parent project, while the ACP adapter only
// sees the fork mount. Rewrite only a cwd that names the parent or fork itself;
// an unrelated host path must not silently become the fork's authority.
func forkACPFromEditor(line []byte, repo, workspace string) (handled bool, response, rewritten []byte, restart bool) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Cwd string `json:"cwd"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &request) != nil || request.Params.Cwd == "" {
		return false, nil, nil, false
	}
	switch request.Method {
	case "session/new", "session/load", "session/resume":
	default:
		return false, nil, nil, false
	}
	cwdInfo, err := os.Stat(request.Params.Cwd)
	if err != nil || !cwdInfo.IsDir() {
		return true, forkACPBadCwd(request.ID, workspace), nil, false
	}
	parentInfo, parentErr := os.Stat(repo)
	forkInfo, forkErr := os.Stat(workspace)
	if parentErr != nil || forkErr != nil || (!os.SameFile(cwdInfo, parentInfo) && !os.SameFile(cwdInfo, forkInfo)) {
		return true, forkACPBadCwd(request.ID, workspace), nil, false
	}
	var top map[string]json.RawMessage
	var params map[string]json.RawMessage
	if json.Unmarshal(line, &top) != nil || json.Unmarshal(top["params"], &params) != nil {
		return false, nil, nil, false
	}
	params["cwd"], _ = json.Marshal(workspace)
	top["params"], _ = json.Marshal(params)
	rewritten, _ = json.Marshal(top)
	return false, nil, append(rewritten, '\n'), false
}

func forkACPBadCwd(id json.RawMessage, workspace string) []byte {
	message := fmt.Sprintf("This fork editor session uses %s. Open its parent project in your editor, then reconnect.", filepath.Clean(workspace))
	response, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32602, "message": message},
	})
	return append(response, '\n')
}
