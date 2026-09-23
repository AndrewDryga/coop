package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
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

// RunningWritableBindSourcesByLabels inventories live Compose sidecars that can replace host
// paths while Coop validates a new launch. Named-volume backing paths are not repository writes.
func (r Runtime) RunningWritableBindSourcesByLabels(ctx context.Context, labels map[string]string) ([]string, error) {
	if r.kind() == runtimeAppleContainer {
		return nil, fmt.Errorf("mount inspection is unsupported by %s", r.Name)
	}
	var docker *Docker
	if r.kind() == runtimeDocker && r.composeEndpoint != "" {
		var err error
		docker, err = bindDocker(ctx, r, r.composeEndpoint, r.composeDaemon, false)
		if err != nil {
			return nil, fmt.Errorf("bind running service inventory: %w", err)
		}
		defer docker.Close()
	}
	filters := make([]string, 0, len(labels))
	for key, value := range labels {
		filter := "label=" + key
		if value != "" {
			filter += "=" + value
		}
		filters = append(filters, filter)
	}
	sort.Strings(filters)
	var ids []string
	var err error
	if docker == nil {
		ids, err = r.containerIDsContext(ctx, false, filters...)
	} else {
		args := []string{"ps", "-q"}
		for _, filter := range filters {
			args = append(args, "--filter", filter)
		}
		var out []byte
		out, err = docker.output(ctx, 1<<20, args...)
		ids = strings.Fields(string(out))
	}
	if err != nil {
		return nil, fmt.Errorf("list running Compose containers: %w", err)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		args := []string{"inspect", "--format", "{{json .Mounts}}", id}
		var out []byte
		if docker == nil {
			out, err = contextCommand(ctx, r.Name, args...).CombinedOutput()
		} else {
			out, err = docker.output(ctx, 1<<20, args...)
		}
		if err != nil {
			return nil, fmt.Errorf("inspect running Compose container %s: %w", id, commandOutputError(err, out))
		}
		var mounts []struct {
			Type   string `json:"Type"`
			Source string `json:"Source"`
			RW     bool   `json:"RW"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(out), &mounts); err != nil {
			return nil, fmt.Errorf("decode running Compose mounts for %s: %w", id, err)
		}
		for _, mount := range mounts {
			if mount.Type == "bind" && mount.RW && mount.Source != "" {
				seen[mount.Source] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for source := range seen {
		out = append(out, source)
	}
	sort.Strings(out)
	return out, nil
}
