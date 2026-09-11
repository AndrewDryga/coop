package cli

import (
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

// extractExposureFlags strips coop's own --readonly/--bare from a launch's arguments and returns
// the execution mode they select — normal when neither is present. They are exclusive: a run is
// one mode, fixed at creation, so both is a contradiction and so is one twice. command is the
// owning command path a refusal names ("coop claude"). Parsing stops at `--`: everything after it
// belongs to the agent or command, where a repeated flag is none of coop's business.
func extractExposureFlags(command string, args []string) (agents.ExecutionMode, []string, error) {
	mode := agents.ModeNormal
	rest := make([]string, 0, len(args))
	for i, arg := range args {
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		var next agents.ExecutionMode
		switch arg {
		case "--readonly":
			next = agents.ModeReadOnly
		case "--bare":
			next = agents.ModeBare
		default:
			rest = append(rest, arg)
			continue
		}
		switch mode {
		case agents.ModeNormal:
			mode = next
		case next:
			return "", nil, ui.RepeatedOption(arg, command)
		default:
			return "", nil, ui.ConflictingOptions("--"+string(mode), arg, command)
		}
	}
	return mode, rest, nil
}

// takeExposureFlags remembers the mode for the launch this command performs, like takeNetworkFlags
// does for the network flags. command is the invocation as a person typed it ("coop claude",
// "coop run"), so a refusal names the command they ran and links to ITS page.
func (a *app) takeExposureFlags(command string, args []string) ([]string, error) {
	mode, rest, err := extractExposureFlags(command, args)
	if err != nil {
		return nil, err
	}
	a.mode = mode
	return rest, nil
}
