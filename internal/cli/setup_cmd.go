package cli

import (
	"context"
	"os"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

var setupCommands = []string{"apparmor"}

// cmdSetup prepares this machine for optional Coop features a person opts into.
func (a *app) cmdSetup(args []string) (int, error) {
	if len(args) == 0 {
		return helpForPath([]string{"setup"}, a.cfg, false)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "apparmor":
		if err := rejectArgs("setup apparmor", rest); err != nil {
			return 2, err
		}
		return a.cmdSetupAppArmor()
	default:
		return 2, unknownSubcommandErr("setup", verb, setupCommands)
	}
}

func (a *app) cmdSetupAppArmor() (int, error) {
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	if err := box.InstallAppArmorProfile(context.Background(), a.rt, os.Stderr); err != nil {
		return 1, err
	}
	ui.OK("The filesystem protection helper now runs under Coop's AppArmor profile")
	return 0, nil
}
