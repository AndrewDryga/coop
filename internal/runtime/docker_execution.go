package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// CopyFile streams one bounded regular file from an exact owned container. The
// caller discards the destination unless the entire single-entry archive passes.
func (d *Docker) CopyFile(ctx context.Context, ref DockerRef, source string, dst io.Writer, limit int64) error {
	_, present, err := d.InspectContainer(ctx, ref)
	if err != nil || ref.ID == "" || !present || !strings.HasPrefix(source, "/") || strings.ContainsAny(source, "\x00\r\n") || dst == nil || limit < 1 || limit > maxDockerFileEvidence {
		return errors.Join(errors.New("Docker file copy source unavailable"), err)
	}
	return d.read(ctx, fileEvidenceTimeout, func(stream io.Reader) error {
		reader := tar.NewReader(stream)
		header, err := reader.Next()
		if err != nil || header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > limit {
			return errors.New("Docker file copy requires one bounded regular file")
		}
		copied, err := io.Copy(dst, reader)
		if err != nil || copied != header.Size {
			return errors.Join(errors.New("Docker file copy was incomplete"), err)
		}
		if _, err := reader.Next(); !errors.Is(err, io.EOF) {
			return errors.New("Docker file copy contained another entry")
		}
		return nil
	}, "container", "cp", ref.ID+":"+source, "-")
}

type DockerHealthcheck struct {
	Test                                          []string
	Interval, Timeout, StartPeriod, StartInterval time.Duration
	Retries                                       int
}

type DockerImageExecution struct {
	ID                     string
	Entrypoint, Cmd, Shell []string
	Healthcheck            *DockerHealthcheck
	Volumes                map[string]struct{}
}

// ImageExecution records the immutable image's startup defaults before a trusted
// guard replaces them. Omitted and empty command fields retain Docker semantics.
func (d *Docker) ImageExecution(ctx context.Context, id string) (DockerImageExecution, error) {
	var value DockerImageExecution
	if !strings.HasPrefix(id, "sha256:") || !dockerHexID(strings.TrimPrefix(id, "sha256:")) {
		return value, errors.New("startup inspection requires an immutable Docker image")
	}
	image, err := d.inspectImageJSON(ctx, id)
	if err != nil {
		return value, err
	}
	c := image.Config
	value = DockerImageExecution{ID: image.ID, Entrypoint: c.Entrypoint, Cmd: c.Cmd, Shell: c.Shell, Healthcheck: c.Healthcheck, Volumes: c.Volumes}
	if value.ID != id || !dockerImageProjectionFits(value) {
		return DockerImageExecution{}, errors.New("invalid Docker image startup observation")
	}
	return value, nil
}

// ContainerLogs is bounded observation of an exact owned helper, including an
// exited helper whose admission error must remain available to the caller.
func (d *Docker) ContainerLogs(ctx context.Context, ref DockerRef, tail, limit int) ([]byte, error) {
	_, data, err := d.ObserveContainerLogs(ctx, ref, tail, limit)
	return data, err
}

// ObserveContainerLogs returns the same fresh ownership-checked state used to
// authorize reading the logs, avoiding a second inspect in readiness polling.
func (d *Docker) ObserveContainerLogs(ctx context.Context, ref DockerRef, tail, limit int) (DockerContainer, []byte, error) {
	if ref.ID == "" || tail < 1 || tail > 1000 || limit < 1 || limit > 4<<20 {
		return DockerContainer{}, nil, errors.New("invalid Docker helper log observation")
	}
	value, present, err := d.InspectContainer(ctx, ref)
	if err != nil || !present {
		return value, nil, errors.Join(errors.New("Docker helper is unavailable"), err)
	}
	// Docker routes container stderr to CLI stderr even when logs succeeds.
	// It shares the same bound; ownership was checked before reading either.
	if d.closed.Load() {
		return value, nil, errDockerCommandNotStarted
	}
	data, err := dockerCommandOutput(ctx, d.binary, d.env, dockerControlTimeout, limit, true, "--config", d.clientConfig, "--host", d.endpoint, "container", "logs", "--tail", strconv.Itoa(tail), ref.ID)
	return value, data, err
}

const dockerCgroupFormat = `{"ID":{{json .Id}},"Parent":{{json (index .HostConfig "CgroupParent")}}}`
const dockerCgroupBatch = 128

// CgroupParentUnused includes stopped containers: Docker can resolve their
// retained parent again on restart, even while its cgroup currently has no tasks.
// The caller serializes creation and retirement for this parent.
func (d *Docker) CgroupParentUnused(ctx context.Context, parent string) error {
	if !validFilesystemCgroupParent(parent) {
		return errors.New("invalid filesystem cgroup parent")
	}
	// Other admissions can retire unrelated containers while this inventory is
	// inspected. One deadline bounds reconciliation, rather than resetting it
	// on each changed inventory.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ids, err := d.boundContainerIDs(ctx)
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("Docker filesystem consumer inventory did not stabilize: %w", err)
		}
		restart := false
		for start := 0; start < len(ids); start += dockerCgroupBatch {
			batch := ids[start:min(start+dockerCgroupBatch, len(ids))]
			args := append([]string{"container", "inspect", "--format", dockerCgroupFormat}, batch...)
			data, err := d.output(ctx, dockerCgroupBatch*1024, args...)
			if err != nil {
				next, observeErr := d.boundContainerIDs(ctx)
				if observeErr != nil {
					return errors.Join(fmt.Errorf("inspect Docker filesystem consumers: %w", err), observeErr)
				}
				remaining := make(map[string]bool, len(next))
				for _, id := range next {
					remaining[id] = true
				}
				for _, id := range batch {
					if !remaining[id] {
						restart = true
						break
					}
				}
				if !restart {
					return fmt.Errorf("inspect Docker filesystem consumers: %w", err)
				}
				// Only exact disappearance explains this failed command. Discard
				// all earlier batches and include every newly listed container.
				ids = next
				break
			}
			if err := checkCgroupParents(data, batch, parent); err != nil {
				return err
			}
		}
		if !restart {
			return d.Verify(ctx)
		}
	}
}

func checkCgroupParents(data []byte, ids []string, parent string) error {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	if len(wanted) != len(ids) {
		return errors.New("duplicate Docker cgroup inventory ID")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var row struct {
			ID     string
			Parent *string
		}
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || !wanted[row.ID] || row.Parent == nil {
			return errors.New("invalid Docker cgroup-parent observation")
		}
		delete(wanted, row.ID)
		observed, target := strings.TrimPrefix(*row.Parent, "/"), strings.TrimPrefix(parent, "/")
		if observed == target || strings.HasPrefix(observed, target+"/") {
			return errors.New("filesystem protection retained for a restartable Docker container")
		}
	}
	if len(wanted) != 0 {
		return errors.New("incomplete Docker cgroup-parent observation")
	}
	return nil
}

// A controller root or its single admission child. Descendant consumers are
// checked by bounded path prefix, including stopped/restartable containers.
func validFilesystemCgroupParent(parent string) bool {
	if !strings.HasPrefix(parent, "/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(parent, "/"), "/")
	valid := func(name, prefix string) bool {
		suffix := strings.TrimPrefix(name, prefix)
		return strings.HasPrefix(name, prefix) && len(suffix) == 32 && strings.Trim(suffix, "0123456789abcdef") == ""
	}
	if len(parts) == 1 {
		return valid(parts[0], "coop-fs-") || valid(parts[0], "coop-fs-controller-")
	}
	return len(parts) == 2 && valid(parts[0], "coop-fs-controller-") && valid(parts[1], "coop-fs-")
}
