package box

import (
	"context"
	"errors"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

// A Docker shared with other sessions can take many seconds to answer while they start and remove
// containers, and a container removed between listing and inspecting it fails that one pass. Neither
// says anything about a saved sign-in, so a launch asks again from the start before refusing. Only a
// whole pass counts: nothing from a failed one is kept. These are variables so a test need not wait.
var (
	dockerListingAttemptLimit = 20 * time.Second
	dockerListingLimit        = 45 * time.Second
	dockerListingPause        = time.Second
)

const dockerListingAttempts = 3

// listDockerPatiently fails closed: when no pass completes, the caller gets a refusal and no listing.
// A cancelled caller stops at once, since the person who asked has gone.
func listDockerPatiently(ctx context.Context, list func(context.Context) error) error {
	limit, cancel := context.WithTimeout(ctx, dockerListingLimit)
	defer cancel()
	for attempt := 1; ; attempt++ {
		pass, stop := context.WithTimeout(limit, dockerListingAttemptLimit)
		err := list(pass)
		timedOut := pass.Err() != nil || errors.Is(err, context.DeadlineExceeded)
		stop()
		if err == nil {
			return nil
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		if attempt == dockerListingAttempts || limit.Err() != nil {
			return dockerListingRefusal(timedOut)
		}
		select {
		case <-limit.Done():
		case <-time.After(dockerListingPause):
		}
	}
}

// dockerListingRefusal never carries Docker's own error, since daemon errors can contain secrets. A
// model probe finds this refusal by its cause (safeModelProbeCause in internal/cli/modelscache.go).
func dockerListingRefusal(timedOut bool) error {
	const unchecked = "Coop could not check which containers hold this saved sign-in.\nNothing was changed. "
	if timedOut {
		return ui.CommandFailed("Docker did not answer in time", unchecked+"Try again.")
	}
	return ui.CommandFailed("Docker could not list its containers", unchecked+"Try again when Docker is ready.")
}
