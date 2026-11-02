package runtime

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
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
	if err := d.Verify(ctx); err != nil {
		return value, err
	}
	const format = `{"ID":{{json .Id}},"Entrypoint":{{json (index .Config "Entrypoint")}},"Cmd":{{json (index .Config "Cmd")}},"Shell":{{json (index .Config "Shell")}},"Healthcheck":{{json (index .Config "Healthcheck")}},"Volumes":{{json (index .Config "Volumes")}}}`
	data, err := d.output(ctx, 64<<10, "image", "inspect", "--format", format, id)
	if err != nil {
		return value, err
	}
	if json.Unmarshal(data, &value) != nil || value.ID != id {
		return DockerImageExecution{}, errors.New("invalid Docker image startup observation")
	}
	return value, nil
}

// ContainerLogs is bounded observation of an exact owned helper, including an
// exited helper whose admission error must remain available to the caller.
func (d *Docker) ContainerLogs(ctx context.Context, ref DockerRef, tail, limit int) ([]byte, error) {
	if ref.ID == "" || tail < 1 || tail > 1000 || limit < 1 || limit > 4<<20 {
		return nil, errors.New("invalid Docker helper log observation")
	}
	_, present, err := d.InspectContainer(ctx, ref)
	if err != nil || !present {
		return nil, errors.Join(errors.New("Docker helper is unavailable"), err)
	}
	return d.output(ctx, limit, "container", "logs", "--tail", strconv.Itoa(tail), ref.ID)
}

// CgroupParentUnused includes stopped containers: Docker can resolve their
// retained parent again on restart, even while its cgroup currently has no tasks.
// The caller serializes creation and retirement for this parent.
func (d *Docker) CgroupParentUnused(ctx context.Context, parent string) error {
	if !strings.HasPrefix(parent, "/coop-fs-") || strings.ContainsAny(strings.TrimPrefix(parent, "/"), "/\x00\r\n") {
		return errors.New("invalid filesystem cgroup parent")
	}
	ids, err := d.boundContainerIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		data, err := d.output(ctx, 1024, "container", "inspect", "--format", `{{json (index .HostConfig "CgroupParent")}}`, id)
		if err != nil {
			return err
		}
		var observed string
		if json.Unmarshal(data, &observed) != nil {
			return errors.New("invalid Docker cgroup-parent observation")
		}
		if strings.TrimPrefix(observed, "/") == strings.TrimPrefix(parent, "/") {
			return errors.New("filesystem protection retained for a restartable Docker container")
		}
	}
	return d.Verify(ctx)
}
