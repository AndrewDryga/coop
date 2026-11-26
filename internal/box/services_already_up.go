package box

import (
	"context"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// ServicesUpBehindBoxes is what `coop up` needs to know before it waits for open boxes: when boxes in
// repo hold its service launch lock and the checkout's own Compose project already runs every service
// file declares, with the ports it publishes (stackRunning), it names those boxes and what runs, so
// the command can say so and stop instead of waiting with no limit for a start with nothing to do.
// Nothing here runs `compose up` or opens a bind source, so nothing crosses the barrier the lock guards.
// A free lock, a lock only a command holds, or a stack `up` would change reports false, and the caller
// waits as before: only a start can fix those.
func ServicesUpBehindBoxes(ctx context.Context, rt runtime.Runtime, repo, file string, exposedRoots ...string) (boxes []string, running ServiceStart, ok bool) {
	if taken, err := forkspace.ServiceLaunchTaken(repo); err != nil || !taken {
		return nil, ServiceStart{}, false
	}
	busy, _ := serviceLaunchHolders(repo, false, "", "")
	if len(busy.Boxes) == 0 {
		return nil, ServiceStart{}, false
	}
	started, up := workspaceServicesRunning(ctx, rt, repo, file, exposedRoots...)
	if !up {
		return nil, ServiceStart{}, false
	}
	return busy.Boxes, ServiceStart{Names: started.names, Ports: started.ports}, true
}

// workspaceServicesRunning is a test seam over stackRunning for the checkout's own project.
var workspaceServicesRunning = func(ctx context.Context, rt runtime.Runtime, repo, file string, exposedRoots ...string) (startedServices, bool) {
	return stackRunning(ctx, rt, repo, file, "", false, exposedRoots...)
}
