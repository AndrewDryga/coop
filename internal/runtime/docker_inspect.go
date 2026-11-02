package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Decode only the fields used for custody and security checks. In particular,
// image-provided environment values never enter an observation or diagnostic.
type dockerContainerJSON struct {
	ID, Name, Image string
	RestartCount    int
	State           *DockerContainerState
	Mounts          []DockerMount
	Config          *struct {
		User                   string
		Labels                 map[string]string
		Tty, OpenStdin         bool
		Entrypoint, Cmd, Shell []string
		Healthcheck            *DockerHealthcheck
	}
	HostConfig *struct {
		NetworkMode, PidMode, IpcMode, UsernsMode, CgroupnsMode, CgroupParent string
		CapAdd, CapDrop, SecurityOpt                                          []string
		Privileged, AutoRemove, ReadonlyRootfs                                bool
		RestartPolicy                                                         struct{ Name string }
		Tmpfs                                                                 map[string]string
	}
}

func (d *Docker) inspectContainerJSON(ctx context.Context, target string) (DockerContainer, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if d.closed.Load() {
		return DockerContainer{}, false, errors.New("Docker lifecycle binding is closed")
	}
	response, err := d.daemonGet(ctx, "/containers/"+url.PathEscape(target)+"/json")
	if err != nil {
		return DockerContainer{}, false, errors.Join(errors.New("Docker container observation unavailable"), ctx.Err())
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDockerContainerObservation+1))
	if err != nil {
		return DockerContainer{}, false, errors.Join(errors.New("Docker container observation incomplete"), ctx.Err())
	}
	if len(data) > maxDockerContainerObservation {
		return DockerContainer{}, false, errors.New("Docker observation exceeded its bound; outcome unknown")
	}
	if err := d.Verify(ctx); err != nil {
		return DockerContainer{}, false, err
	}
	if response.StatusCode == http.StatusNotFound {
		return DockerContainer{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return DockerContainer{}, false, errors.New("Docker container observation unavailable")
	}
	var value dockerContainerJSON
	if json.Unmarshal(data, &value) != nil || value.Config == nil || value.HostConfig == nil || value.State == nil {
		return DockerContainer{}, false, errors.New("invalid Docker container observation")
	}
	c, h := value.Config, value.HostConfig
	return DockerContainer{
		ID: value.ID, Name: value.Name, Image: value.Image, RestartCount: value.RestartCount,
		State: *value.State, Mounts: value.Mounts, User: c.User, Labels: c.Labels,
		Tty: c.Tty, OpenStdin: c.OpenStdin, Entrypoint: c.Entrypoint, Cmd: c.Cmd, Shell: c.Shell, Healthcheck: c.Healthcheck,
		NetworkMode: h.NetworkMode, PidMode: h.PidMode, IpcMode: h.IpcMode, UsernsMode: h.UsernsMode,
		CgroupnsMode: h.CgroupnsMode, CgroupParent: h.CgroupParent, CapAdd: h.CapAdd, CapDrop: h.CapDrop,
		SecurityOpt: h.SecurityOpt, Privileged: h.Privileged, AutoRemove: h.AutoRemove, ReadonlyRootfs: h.ReadonlyRootfs,
		RestartPolicy: h.RestartPolicy.Name, Tmpfs: h.Tmpfs,
	}, true, nil
}
