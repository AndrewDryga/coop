package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// DefaultMountType is a dialect property, not a networking capability claim.
func (r Runtime) DefaultMountType() string {
	if r.kind() == runtimeAppleContainer {
		return "bind"
	}
	return "volume"
}

type volumeDefinition struct {
	Name, Driver, Scope, Mountpoint string
	Options                         map[string]string
}

// BindSources retain paths the existing driver definition will consume. Callers
// must validate their canonical spelling and independent parents; changing the
// returned string cannot change a named volume's stored driver configuration.
type VolumeExposure struct {
	Sources     []string
	BindSources []string
	// ManagedParents are conservative exposure boundaries for daemon-private
	// mountpoints that the host owner cannot traverse. Never inferred for binds.
	ManagedParents []string
}

// ExistingNamedVolumeExposure never creates a volume. A positively observed
// absence may be skipped; failed or malformed inventory remains unknown/error.
func (r Runtime) ExistingNamedVolumeExposure(ctx context.Context, names []string) (VolumeExposure, error) {
	return (volumeReader{r.kind(), r.volumeOutput}).existing(ctx, names)
}

// ExistingNamedVolumeExposure inventories only this bound daemon. Mutable
// Docker context and proxy settings cannot redirect an authority exposure check.
func (d *Docker) ExistingNamedVolumeExposure(ctx context.Context, names []string) (VolumeExposure, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := d.Verify(ctx); err != nil {
		return VolumeExposure{}, err
	}
	exposure, err := (volumeReader{runtimeDocker, d.output}).existing(ctx, names)
	if err != nil {
		return VolumeExposure{}, err
	}
	if err := d.Verify(ctx); err != nil {
		return VolumeExposure{}, err
	}
	return exposure, nil
}

// PrepareOrdinaryDockerVolumes performs the ordinary run's default-local volume
// creation on one previously inspected daemon. This explicit provisioning call
// grants no container launch authority and never replaces an existing volume.
func PrepareOrdinaryDockerVolumes(ctx context.Context, rt Runtime, endpoint, daemonID string, names []string) (VolumeExposure, error) {
	if endpoint == "" || daemonID == "" {
		return VolumeExposure{}, errors.New("ordinary volume preparation requires an inspected Docker identity")
	}
	docker, err := BindDocker(ctx, rt, endpoint, daemonID)
	if err != nil {
		return VolumeExposure{}, err
	}
	defer docker.Close()
	exposure, err := (volumeReader{runtimeDocker, docker.output}).named(ctx, names, true)
	if err != nil {
		return VolumeExposure{}, err
	}
	if err := docker.Verify(ctx); err != nil {
		return VolumeExposure{}, err
	}
	return exposure, nil
}

type volumeReader struct {
	dialect runtimeKind
	output  func(context.Context, int, ...string) ([]byte, error)
}

func (r volumeReader) existing(ctx context.Context, names []string) (VolumeExposure, error) {
	if len(names) > 64 {
		return VolumeExposure{}, errors.New("runtime volume inventory exceeds its limit")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var existing []string
	for _, name := range names {
		if !volumeName(name) {
			return VolumeExposure{}, errors.New("invalid selected runtime volume name")
		}
		absent, err := r.namedVolumeAbsent(ctx, name)
		if err != nil {
			return VolumeExposure{}, err
		}
		if !absent {
			existing = append(existing, name)
		}
	}
	return r.named(ctx, existing, false)
}

// NamedVolumeExposure returns the backing paths of exactly the selected local
// volumes. With prepare, confirmed missing volumes get the same default local
// creation an ordinary run would perform, then inspection BEFORE owner-key
// publication. Existing volumes are never replaced. Final checks pass false.
func (r Runtime) NamedVolumeExposure(ctx context.Context, names []string, prepare bool) (VolumeExposure, error) {
	return (volumeReader{r.kind(), r.volumeOutput}).named(ctx, names, prepare)
}

func (r volumeReader) named(ctx context.Context, names []string, prepare bool) (VolumeExposure, error) {
	if len(names) == 0 {
		return VolumeExposure{}, nil
	}
	if len(names) > 64 {
		return VolumeExposure{}, errors.New("runtime volume inventory exceeds its limit")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var exposure VolumeExposure
	var seen []string
	for _, name := range names {
		if !volumeName(name) {
			return VolumeExposure{}, errors.New("invalid selected runtime volume name")
		}
		if slices.Contains(seen, name) {
			continue
		}
		seen = append(seen, name)
		definition, err := r.readVolumeDefinition(ctx, name)
		if err != nil && prepare {
			absent, absenceErr := r.namedVolumeAbsent(ctx, name)
			if absenceErr != nil || !absent {
				return VolumeExposure{}, errors.New("selected runtime volume cannot be inspected")
			}
			args := []string{"volume", "create"}
			if r.dialect != runtimeAppleContainer {
				args = append(args, "--driver", "local")
			}
			args = append(args, name)
			if _, err := r.output(ctx, 1024, args...); err != nil {
				return VolumeExposure{}, err
			}
			definition, err = r.readVolumeDefinition(ctx, name)
		}
		if err != nil {
			return VolumeExposure{}, err
		}
		roots, err := volumeBackingPaths(definition, r.dialect == runtimeAppleContainer)
		if err != nil {
			return VolumeExposure{}, err
		}
		mountpoint, err := volumeMountpointExposure(roots[0])
		if err != nil {
			return VolumeExposure{}, err
		}
		if mountpoint != roots[0] {
			exposure.ManagedParents = append(exposure.ManagedParents, mountpoint)
			roots[0] = mountpoint
		}
		exposure.Sources = append(exposure.Sources, roots...)
		if len(roots) > 1 {
			exposure.BindSources = append(exposure.BindSources, roots[1:]...)
		}
	}
	return exposure, nil
}

// Rootful daemons keep ordinary volume data beneath a directory a docker-group
// client cannot search. Protect its whole existing parent instead of waiving
// exposure checking or treating EACCES as absence. The root administrator and
// runtime are trusted; an owner-controlled inaccessible directory gets no waiver.
func volumeMountpointExposure(path string) (string, error) {
	if _, err := filepath.EvalSymlinks(path); !errors.Is(err, os.ErrPermission) {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return path, nil
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			info, statErr := os.Lstat(resolved)
			if statErr == nil {
				stat, ok := info.Sys().(*syscall.Stat_t)
				if ok && os.Getuid() != 0 && stat.Uid == 0 && info.IsDir() && info.Mode().Perm()&0022 == 0 {
					return resolved, nil
				}
			}
			return "", errors.New("runtime volume mountpoint is inaccessible outside a protected daemon directory")
		}
		if !errors.Is(err, os.ErrPermission) || filepath.Dir(parent) == parent {
			return "", errors.New("runtime volume mountpoint exposure is unavailable")
		}
	}
}

func (r Runtime) volumeOutput(ctx context.Context, limit int, args ...string) ([]byte, error) {
	data, err := dockerOutput(ctx, r.Name, interruptibleProcessEnvironment(), limit, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("runtime volume observation failed; exposure is unknown")
	}
	return data, nil
}

func (r volumeReader) readVolumeDefinition(ctx context.Context, name string) (volumeDefinition, error) {
	fail := func() (volumeDefinition, error) {
		return volumeDefinition{}, errors.New("selected runtime volume definition is invalid or unavailable")
	}
	args := []string{"volume", "inspect"}
	if r.dialect != runtimeAppleContainer {
		args = append(args, "--format", `{"Name":{{json .Name}},"Driver":{{json .Driver}},"Scope":{{json .Scope}},"Mountpoint":{{json .Mountpoint}},"Options":{{json .Options}}}`)
	}
	data, err := r.output(ctx, 64<<10, append(args, name)...)
	if err != nil {
		return fail()
	}
	var value volumeDefinition
	if r.dialect == runtimeAppleContainer {
		var values []struct {
			ID            string `json:"id"`
			Configuration struct {
				Name, Driver, Format, Source string
				Options                      map[string]string
			} `json:"configuration"`
		}
		if json.Unmarshal(data, &values) != nil || len(values) != 1 || values[0].ID != name || values[0].Configuration.Format != "ext4" {
			return fail()
		}
		c := values[0].Configuration
		value = volumeDefinition{Name: c.Name, Driver: c.Driver, Scope: "local", Mountpoint: c.Source, Options: c.Options}
	} else if json.Unmarshal(data, &value) != nil {
		return fail()
	}
	if value.Name != name || len(value.Options) > 32 {
		return fail()
	}
	return value, nil
}

func (r volumeReader) namedVolumeAbsent(ctx context.Context, name string) (bool, error) {
	args := []string{"volume", "ls", "--quiet"}
	if r.dialect != runtimeAppleContainer {
		args = append(args, "--filter", "name="+name)
	}
	data, err := r.output(ctx, 64<<10, args...)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Fields(string(data)) {
		if !volumeName(line) {
			return false, errors.New("invalid runtime volume absence observation")
		}
		if line == name {
			return false, nil
		}
	}
	return true, nil
}

func volumeName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for i, c := range name {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && strings.ContainsRune("_.-", c) {
			continue
		}
		return false
	}
	return true
}

func volumeBackingPaths(value volumeDefinition, apple bool) ([]string, error) {
	fail := func() ([]string, error) {
		return nil, errors.New("runtime volume uses an unsupported host exposure; retain it and choose a plain local volume or explicit bind")
	}
	validPath := func(path string) bool {
		return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
	}
	if value.Driver != "local" || value.Scope != "local" || !validPath(value.Mountpoint) {
		return fail()
	}
	paths := []string{value.Mountpoint}
	for key := range value.Options {
		allowed := []string{"type", "device", "o", "uid", "gid", "size", "inodes", "copy", "nocopy"}
		if apple {
			allowed = []string{"size", "journal"}
		}
		if !slices.Contains(allowed, key) {
			return fail()
		}
	}
	if apple {
		return paths, nil
	}
	device, fs := value.Options["device"], value.Options["type"]
	if device == "" && fs == "" {
		return paths, nil
	}
	if fs == "tmpfs" && (device == "tmpfs" || device == "") {
		return paths, nil
	}
	if !validPath(device) || fs != "" && fs != "none" {
		return fail()
	}
	options := strings.Split(value.Options["o"], ",")
	if !slices.Contains(options, "bind") && !slices.Contains(options, "rbind") {
		return fail()
	}
	return append(paths, device), nil
}
