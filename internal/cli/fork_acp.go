package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
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
// choices remain available. Its child still takes the
// forkACP path, which owns the parent's image, fork generation and network policy.
func (a *app) superviseForkACP(repo, workspace, name string, target agents.Target, peers []agents.Target, capture *box.CapturedEgress) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}
	superID, err := newSupervisorID()
	if err != nil {
		return 1, err
	}
	if account := a.cfg.ActiveProfile(target.Provider); account != "" {
		target.Accounts = []string{account}
	}
	a.acpCapture = capture
	a.acpPeers = peers
	if capture != nil {
		a.acpNetworkTargets, _, err = a.acpFilteredSpawnScope(target, "")
		if err != nil {
			return 1, err
		}
	}
	inner := []string{"fork", name, "acp", target.String()}
	for _, peer := range peers {
		inner = append(inner, "--peer", peer.String())
	}
	networkArgs, err := a.network.args()
	if err != nil {
		return 2, err
	}
	inner = append(inner, networkArgs...)

	ctrl := newForkACPControl(a.cfg, target, workspace)
	hooks := fixedForkACPHooks(ctrl, repo, workspace)
	factory := func(ctx context.Context) (*acpproxy.Child, error) {
		return a.spawnBox(ctx, self, inner, superID, nil, target, "", false, os.Stderr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err = acpproxy.RunWith(ctx, os.Stdin, os.Stdout, factory, hooks, acpproxy.RunOpts{})
	cleanupErr := a.reapACPBoxes(superID)
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
