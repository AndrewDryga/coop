package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdNet is the restricted-networking family. It is deliberately host-wide: the
// per-host preflight belongs to the machine and its Docker daemon, not to
// whatever directory you happened to run it from.
func (a *app) cmdNet(args []string) (int, error) {
	if len(args) == 0 {
		return groupHelp("net")
	}
	switch args[0] {
	case "setup":
		if err := rejectArgs("net setup", args[1:]); err != nil {
			return 2, err
		}
		return a.cmdNetSetup()
	default:
		return 2, fmt.Errorf("net: unknown command %q", args[0])
	}
}

func (a *app) cmdNetSetup() (int, error) {
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	ui.Info("setting up restricted networking for this host's container runtime")
	if _, err := box.SetupNetwork(context.Background(), a.cfg, a.rt, os.Stderr, os.Stderr); err != nil {
		return 1, err
	}
	ui.Steps("coop run --egress filtered --allow-domain example.com -- curl https://example.com")
	return 0, nil
}
