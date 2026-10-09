package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
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
	return r.WritableBindSourcesByLabels(ctx, labels, false)
}

// WritableBindSourcesByLabels includes stopped/restartable containers when all
// is true. Credential cutover cannot treat an exited writer as permanently gone.
func (r Runtime) WritableBindSourcesByLabels(ctx context.Context, labels map[string]string, all bool) ([]string, error) {
	return r.bindSourcesByLabels(ctx, labels, all, true)
}

// BindSourcesByLabels also includes read-only copies of credential authority:
// possessing a refresh grant is enough to rotate it without filesystem writes.
func (r Runtime) BindSourcesByLabels(ctx context.Context, labels map[string]string) ([]string, error) {
	return r.bindSourcesByLabels(ctx, labels, true, false)
}

func (r Runtime) bindSourcesByLabels(ctx context.Context, labels map[string]string, all, writable bool) ([]string, error) {
	mounts, err := r.BindMountsByLabels(ctx, labels, all, writable)
	if err != nil {
		return nil, err
	}
	sources := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		sources = append(sources, mount.Source)
	}
	return sources, nil
}

// BindMount records whether every observed container using this source was created/exited.
// An unresolved live bind may retain an inode even after its source name disappears.
type BindMount struct {
	Source  string
	Stopped bool
}

// BindMountsByLabels retains lifecycle evidence from the same complete mount observation.
// Other runtimes cannot prove stopped custody and conservatively leave Stopped false.
func (r Runtime) BindMountsByLabels(ctx context.Context, labels map[string]string, all, writable bool) ([]BindMount, error) {
	if r.kind() == runtimeAppleContainer {
		return nil, fmt.Errorf("mount inspection is unsupported by %s", r.Name)
	}
	var docker *Docker
	if r.kind() == runtimeDocker && (r.composeEndpoint != "" || all) {
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
		ids, err = r.containerIDsContext(ctx, all, filters...)
	} else {
		args := []string{"ps", "-q", "--no-trunc"}
		if all {
			args = append(args, "-a")
		}
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
	if docker != nil {
		if err := docker.Verify(ctx); err != nil {
			return nil, err
		}
		if err := docker.collectBindSources(ctx, ids, writable, seen); err != nil {
			return nil, err
		}
		ids = nil
	}
	for _, id := range ids {
		args := []string{"inspect", "--format", "{{json .Mounts}}", id}
		out, err := contextCommand(ctx, r.Name, args...).CombinedOutput()
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
			if mount.Type == "bind" && (!writable || mount.RW) && mount.Source != "" {
				seen[mount.Source] = false
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for source := range seen {
		keys = append(keys, source)
	}
	sort.Strings(keys)
	out := make([]BindMount, 0, len(keys))
	for _, source := range keys {
		out = append(out, BindMount{Source: source, Stopped: seen[source]})
	}
	return out, nil
}

// Batch process startup, not custody checks: every full ID must yield exactly one mount record.
// Streaming keeps the existing per-container limit without imposing it on the whole inventory.
func (d *Docker) collectBindSources(ctx context.Context, ids []string, writable bool, sources map[string]bool) error {
	const batchSize = 32
	const mountsLimit = 1 << 20
	listed := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !dockerHexID(id) || listed[id] {
			return fmt.Errorf("container inventory contains an invalid or repeated identity")
		}
		listed[id] = true
	}
	for start := 0; start < len(ids); start += batchSize {
		if err := d.Verify(ctx); err != nil {
			return err
		}
		batch := ids[start:min(start+batchSize, len(ids))]
		expected := make(map[string]bool, len(batch))
		for _, id := range batch {
			expected[id] = true
		}
		args := append([]string{"inspect", "--type", "container", "--format", `{"ID":{{json .Id}},"Status":{{json .State.Status}},"Mounts":{{json .Mounts}}}`}, batch...)
		err := d.read(ctx, 15*time.Second, func(reader io.Reader) error {
			scanner := bufio.NewScanner(reader)
			scanner.Buffer(make([]byte, 4096), mountsLimit+256)
			for scanner.Scan() {
				var record struct {
					ID     string          `json:"ID"`
					Status string          `json:"Status"`
					Mounts json.RawMessage `json:"Mounts"`
				}
				decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&record); err != nil {
					return fmt.Errorf("container mount inventory is malformed")
				}
				if decoder.Decode(new(any)) != io.EOF || !expected[record.ID] || len(record.Mounts) == 0 || len(record.Mounts) > mountsLimit {
					return fmt.Errorf("container mount inventory is incomplete or mismatched")
				}
				delete(expected, record.ID)
				switch record.Status {
				case "created", "exited", "running", "paused", "restarting", "removing", "dead":
				default:
					return fmt.Errorf("container lifecycle inventory is incomplete")
				}
				var mounts []struct {
					Type   string `json:"Type"`
					Source string `json:"Source"`
					RW     *bool  `json:"RW"`
				}
				if err := json.Unmarshal(record.Mounts, &mounts); err != nil {
					return fmt.Errorf("container mount inventory is malformed")
				}
				for _, mount := range mounts {
					if mount.Type != "bind" {
						continue
					}
					if mount.RW == nil || mount.Source == "" {
						return fmt.Errorf("container bind inventory is incomplete")
					}
					if !writable || *mount.RW {
						stopped := record.Status == "created" || record.Status == "exited"
						if previous, exists := sources[mount.Source]; exists {
							stopped = stopped && previous
						}
						sources[mount.Source] = stopped
					}
				}
			}
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("container mount inventory could not be read within its bound")
			}
			if len(expected) != 0 {
				return fmt.Errorf("container mount inventory is incomplete")
			}
			return nil
		}, args...)
		if err != nil {
			return fmt.Errorf("inspect container mount inventory: %w", err)
		}
		if err := d.Verify(ctx); err != nil {
			return err
		}
	}
	return d.Verify(ctx)
}
