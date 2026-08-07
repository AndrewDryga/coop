package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MountSourcesByLabel returns the host paths mounted into every container
// matching key=value, running or stopped.
//
// It exists so cleanup can ask "is this path in use?" instead of "is this path
// old?". Age is a guess about a box's lifetime; a mount is a fact about it.
//
// A query or inspect failure is an error, never an empty result. An empty
// result means "nothing is mounted", and a caller that deletes what is not
// mounted would take a transient docker failure as permission to delete
// everything.
func (r Runtime) MountSourcesByLabel(
	ctx context.Context,
	key, value string,
) (map[string]bool, error) {
	if r.kind() == runtimeAppleContainer {
		return nil, fmt.Errorf("mount inspection is unsupported by %s", r.Name)
	}
	ids, err := r.containerIDsContext(ctx, true, "label="+key+"="+value)
	if err != nil {
		return nil, fmt.Errorf("list matching containers: %w", err)
	}
	sources := make(map[string]bool)
	for _, id := range ids {
		args := []string{"inspect", "--format", "{{json .Mounts}}", id}
		out, err := contextCommand(ctx, r.Name, args...).CombinedOutput()
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return nil, fmt.Errorf(
				"run: %s %s: %w", r.Name, strings.Join(args, " "), commandOutputError(err, out),
			)
		}
		var mounts []struct {
			Source string `json:"Source"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(out), &mounts); err != nil {
			return nil, fmt.Errorf("decode mounts for container %s: %w", id, err)
		}
		for _, mount := range mounts {
			if mount.Source != "" {
				sources[mount.Source] = true
			}
		}
	}
	return sources, nil
}
