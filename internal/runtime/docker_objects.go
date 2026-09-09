package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// DockerRef is exact owner custody, not a discovery filter. Before creation ID
// is empty; once a container ID is known every mutation uses that full ID.
type DockerRef struct {
	Name, ID string
	Labels   map[string]string
}

func (r DockerRef) valid(volume bool) bool {
	if !dockerName(r.Name) || len(r.Labels) == 0 || len(r.Labels) > 32 || r.ID != "" && (volume && r.ID != r.Name || !volume && !dockerHexID(r.ID)) {
		return false
	}
	for key, value := range r.Labels {
		if !dockerToken(key, 128) || !dockerToken(value, 512) || strings.Contains(key, "=") {
			return false
		}
	}
	return true
}

func (r DockerRef) matches(name, id string, labels map[string]string) bool {
	if name != r.Name || r.ID != "" && id != r.ID {
		return false
	}
	for key, value := range r.Labels {
		if labels[key] != value {
			return false
		}
	}
	return true
}

type DockerContainer struct {
	ID, Name, Image, User, NetworkMode, PidMode, IpcMode, UsernsMode, CgroupnsMode string
	RestartPolicy                                                                  string
	Labels                                                                         map[string]string
	CapAdd, CapDrop, SecurityOpt                                                   []string
	Privileged, AutoRemove, ReadonlyRootfs, Tty, OpenStdin                         bool
	RestartCount                                                                   int
	State                                                                          DockerContainerState
	Mounts                                                                         []DockerMount
	Tmpfs                                                                          map[string]string
}

type DockerContainerState struct {
	Status                string
	Running, Paused       bool
	OOMKilled             bool
	ExitCode              int
	StartedAt, FinishedAt time.Time
}

type DockerMount struct {
	Type, Name, Source, Destination string
	RW                              bool
}

const dockerContainerFormat = `{"ID":{{json .Id}},"Name":{{json .Name}},"Image":{{json .Image}},"User":{{json .Config.User}},
"Labels":{{json .Config.Labels}},"Tty":{{json .Config.Tty}},"OpenStdin":{{json .Config.OpenStdin}},
"NetworkMode":{{json .HostConfig.NetworkMode}},"PidMode":{{json .HostConfig.PidMode}},"IpcMode":{{json .HostConfig.IpcMode}},
"UsernsMode":{{json .HostConfig.UsernsMode}},"CgroupnsMode":{{json .HostConfig.CgroupnsMode}},
"CapAdd":{{json .HostConfig.CapAdd}},"CapDrop":{{json .HostConfig.CapDrop}},"SecurityOpt":{{json .HostConfig.SecurityOpt}},
"Privileged":{{json .HostConfig.Privileged}},"AutoRemove":{{json .HostConfig.AutoRemove}},"ReadonlyRootfs":{{json .HostConfig.ReadonlyRootfs}},
"RestartPolicy":{{json .HostConfig.RestartPolicy.Name}},"RestartCount":{{json .RestartCount}},"Mounts":{{json .Mounts}},"Tmpfs":{{json (index .HostConfig "Tmpfs")}},
"State":{"Status":{{json .State.Status}},"Running":{{json .State.Running}},"Paused":{{json .State.Paused}},"OOMKilled":{{json .State.OOMKilled}},
"ExitCode":{{json .State.ExitCode}},"StartedAt":{{json .State.StartedAt}},"FinishedAt":{{json .State.FinishedAt}}}}`

// InspectContainer distinguishes confirmed current absence from unavailable
// inspection. Absence does not settle a still-in-flight creating intention.
func (d *Docker) InspectContainer(ctx context.Context, ref DockerRef) (DockerContainer, bool, error) {
	if !ref.valid(false) {
		return DockerContainer{}, false, errors.New("invalid exact Docker container reference")
	}
	if err := d.Verify(ctx); err != nil {
		return DockerContainer{}, false, err
	}
	target := ref.ID
	if target == "" {
		target = ref.Name
	}
	data, err := d.output(ctx, 256<<10, "container", "inspect", "--format", dockerContainerFormat, target)
	if err != nil {
		absent, checkErr := d.containerAbsent(ctx, ref)
		if checkErr == nil && absent {
			return DockerContainer{}, false, nil
		}
		return DockerContainer{}, false, errors.Join(err, checkErr)
	}
	var value DockerContainer
	if json.Unmarshal(data, &value) != nil || !dockerHexID(value.ID) || !strings.HasPrefix(value.Name, "/") || len(value.Mounts) > 512 || len(value.Tmpfs) > 32 || len(value.Labels) > 256 || value.RestartCount < 0 ||
		!slices.Contains([]string{"created", "running", "paused", "restarting", "removing", "exited", "dead"}, value.State.Status) {
		return DockerContainer{}, false, errors.New("invalid Docker container observation")
	}
	value.Name = strings.TrimPrefix(value.Name, "/")
	if !ref.matches(value.Name, value.ID, value.Labels) {
		return DockerContainer{}, false, errors.New("Docker container ownership conflict")
	}
	return value, true, nil
}

func (d *Docker) containerAbsent(ctx context.Context, ref DockerRef) (bool, error) {
	if err := d.Verify(ctx); err != nil {
		return false, err
	}
	filter := "name=" + ref.Name
	if ref.ID != "" {
		filter = "id=" + ref.ID
	}
	data, err := d.output(ctx, 64<<10, "container", "ls", "--all", "--no-trunc", "--filter", filter, "--format", `{"name":{{json .Names}},"id":{{json .ID}}}`)
	if err != nil {
		return false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var item struct{ Name, ID string }
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			return true, nil
		} else if err != nil || !dockerHexID(item.ID) || !dockerName(item.Name) {
			return false, errors.New("invalid Docker absence observation")
		}
		if ref.ID != "" && item.ID == ref.ID || ref.ID == "" && item.Name == ref.Name {
			return false, nil
		}
	}
}

type DockerVolume struct {
	Name, Driver, Scope, CreatedAt string
	Labels, Options                map[string]string
}

func (d *Docker) InspectVolume(ctx context.Context, ref DockerRef) (DockerVolume, bool, error) {
	if !ref.valid(true) {
		return DockerVolume{}, false, errors.New("invalid exact Docker volume reference")
	}
	if err := d.Verify(ctx); err != nil {
		return DockerVolume{}, false, err
	}
	data, err := d.output(ctx, 64<<10, "volume", "inspect", "--format", `{"Name":{{json .Name}},"Driver":{{json .Driver}},"Scope":{{json .Scope}},"CreatedAt":{{json .CreatedAt}},"Labels":{{json .Labels}},"Options":{{json .Options}}}`, ref.Name)
	if err != nil {
		absent, checkErr := d.volumeAbsent(ctx, ref.Name)
		if checkErr == nil && absent {
			return DockerVolume{}, false, nil
		}
		return DockerVolume{}, false, errors.Join(err, checkErr)
	}
	var value DockerVolume
	if json.Unmarshal(data, &value) != nil || !ref.matches(value.Name, value.Name, value.Labels) || value.Driver != "local" || value.Scope != "local" || len(value.Options) != 0 || value.CreatedAt == "" {
		return DockerVolume{}, false, errors.New("Docker volume ownership or driver conflict")
	}
	return value, true, nil
}

func (d *Docker) volumeAbsent(ctx context.Context, name string) (bool, error) {
	if err := d.Verify(ctx); err != nil {
		return false, err
	}
	data, err := d.output(ctx, 64<<10, "volume", "ls", "--filter", "name="+name, "--format", "{{json .Name}}")
	if err != nil {
		return false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var item string
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			return true, nil
		} else if err != nil || !dockerName(item) {
			return false, errors.New("invalid Docker volume absence observation")
		}
		if item == name {
			return false, nil
		}
	}
}

func dockerLabelArgs(labels map[string]string) []string {
	var args []string
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--label", key+"="+labels[key])
	}
	return args
}

// CreateVolume never treats an existing volume as a successful fresh create.
// Reconciliation of an ambiguous prior call must use InspectVolume explicitly.
func (d *Docker) CreateVolume(ctx context.Context, ref DockerRef) (DockerVolume, error) {
	if err := d.VerifyLaunch(ctx); err != nil {
		return DockerVolume{}, errors.Join(ErrDockerCreateNotAttempted, err)
	}
	_, present, err := d.InspectVolume(ctx, ref)
	if err != nil || present || ref.ID != "" {
		return DockerVolume{}, errors.Join(ErrDockerCreateNotAttempted, errors.New("Docker volume creation requires a fresh intention"), err)
	}
	args := append([]string{"volume", "create", "--driver", "local"}, dockerLabelArgs(ref.Labels)...)
	data, err := d.output(ctx, 1024, append(args, ref.Name)...)
	if err != nil {
		if errors.Is(err, errDockerCommandNotStarted) {
			err = errors.Join(ErrDockerCreateNotAttempted, err)
		}
		return DockerVolume{}, err
	}
	if strings.TrimSpace(string(data)) != ref.Name {
		return DockerVolume{}, errors.New("Docker volume create outcome is invalid")
	}
	value, present, err := d.InspectVolume(ctx, ref)
	if err != nil || !present {
		return DockerVolume{}, errors.Join(errors.New("Docker volume create outcome is unavailable"), err)
	}
	return value, nil
}

func (d *Docker) RemoveVolume(ctx context.Context, ref DockerRef) error {
	_, present, err := d.InspectVolume(ctx, ref)
	if err != nil || !present {
		return err
	}
	_, removeErr := d.output(ctx, 1024, "volume", "rm", ref.Name)
	absent, err := d.volumeAbsent(ctx, ref.Name)
	if err == nil && absent {
		return nil
	}
	return errors.Join(errors.New("Docker volume cleanup remains pending"), removeErr, err)
}
