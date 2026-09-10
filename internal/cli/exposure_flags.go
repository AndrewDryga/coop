package cli

import (
	"errors"
	"fmt"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// extractExposureFlags strips coop's own --readonly/--bare from a launch's arguments and returns
// the execution mode they select — normal when neither is present. They are exclusive: a run is
// one mode, fixed at creation, so both is a contradiction and so is one twice. Parsing stops at
// `--`: everything after it belongs to the agent or command.
func extractExposureFlags(args []string) (agents.ExecutionMode, []string, error) {
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
			return "", nil, fmt.Errorf("%s may be supplied only once", arg)
		default:
			return "", nil, errors.New("--readonly and --bare are exclusive: a run is one or the other")
		}
	}
	return mode, rest, nil
}

// takeExposureFlags remembers the mode for the launch this command performs, like takeNetworkFlags
// does for the network flags.
func (a *app) takeExposureFlags(args []string) ([]string, error) {
	mode, rest, err := extractExposureFlags(args)
	if err != nil {
		return nil, err
	}
	a.mode = mode
	return rest, nil
}
