package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/fsimage"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// Under Docker's systemd cgroup driver the filesystem helper needs the system bus, which Docker's
// default AppArmor profile refuses. Without `coop setup apparmor` it runs unconfined, as it always
// has; with it, under Coop's own profile, which keeps Docker's default denials (mounts included) and
// allows only the bus calls it makes (see fsfilter.AppArmorPolicy for what that cannot limit). Docker refuses a container whose profile is not loaded, so Coop uses the profile only
// when the kernel lists it.

const appArmorPolicyDir = "/sys/kernel/security/apparmor/policy/profiles"

// hostAppArmor reports whether this host's kernel enforces AppArmor.
func hostAppArmor() bool {
	data, err := os.ReadFile("/sys/kernel/security/lsm")
	return err == nil && slices.Contains(strings.Split(strings.TrimSpace(string(data)), ","), "apparmor")
}

// appArmorProfileLoaded reports whether this Coop's helper profile is loaded. The kernel lists each
// loaded profile as a folder named <profile>.<number>, which any user may read.
func appArmorProfileLoaded() bool {
	entries, err := os.ReadDir(appArmorPolicyDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name, _, ok := strings.Cut(entry.Name(), ".")
		if ok && name == fsfilter.AppArmorProfile {
			return true
		}
	}
	return false
}

// daemonAppArmor reports whether the Docker daemon applies AppArmor profiles. The profile check reads
// this machine's kernel, so a daemon that does not apply them (another machine's) never gets one.
func daemonAppArmor(info runtime.DockerInfo) bool {
	return slices.ContainsFunc(info.SecurityOptions, func(o string) bool { return strings.Contains(o, "name=apparmor") })
}

// filesystemAppArmorNote is what coop doctor says beside kernel protection when the helper could be
// confined and is not.
func filesystemAppArmorNote(controller filesystemControllerRecord) string {
	if !controller.systemd() || controller.AppArmor != "" || !hostAppArmor() {
		return ""
	}
	return "The protection helper runs without AppArmor. To confine it, run: coop setup apparmor"
}

var errAppArmorUnsupported = errors.New("this host does not run AppArmor, so there is nothing to set up")

// InstallAppArmorProfile writes the helper's profile into the host's /etc/apparmor.d, so it loads on
// every boot, and loads it now with the host's own apparmor_parser. Like the rest of filesystem
// protection it works through Docker, in a one-shot release-owned helper, so no sudo is needed: it
// runs as root with only the capabilities loading a profile takes. Older versions of the profile
// are unloaded and removed.
func InstallAppArmorProfile(ctx context.Context, rt runtime.Runtime, progress io.Writer) error {
	if !hostAppArmor() {
		return errAppArmorUnsupported
	}
	endpoint, daemon := rt.ComposeBinding()
	docker, err := runtime.BindDockerForSetup(ctx, rt, endpoint, daemon)
	if err != nil {
		return err
	}
	defer docker.Close()
	if !daemonAppArmor(docker.Info()) {
		return errAppArmorUnsupported
	}
	image, labels, err := docker.Image(ctx, fsimage.Tag())
	if errors.Is(err, runtime.ErrDockerImageMissing) {
		fmt.Fprintln(progress, "Building the filesystem protection helper")
		var log *os.File
		if log, err = os.CreateTemp("", "coop-fs-build-*.log"); err != nil {
			return err
		}
		defer os.Remove(log.Name())
		image, err = fsimage.Build(ctx, docker, log)
		err = errors.Join(err, log.Close())
		if err != nil {
			err = filesystemBuildError(err, log.Name())
		}
	} else if err == nil && labels[fsimage.BuildLabel] != fsimage.Fingerprint() {
		err = errors.New("filesystem helper image has an unexpected build identity")
	}
	if err != nil {
		return err
	}
	ref := filesystemRef("coop-fs-apparmor-"+randid.Hex(16), "apparmor")
	options := []string{"--network", "none", "--user", "0", "--read-only", "--no-healthcheck", "--cap-drop", "ALL",
		"--cap-add", "MAC_ADMIN", "--cap-add", "SYS_CHROOT", "--security-opt", "apparmor=unconfined",
		"--mount", networkMount("bind", "/", "/host", true),
		"--mount", networkMount("bind", "/etc/apparmor.d", "/host-apparmor", false),
		"--mount", networkMount("bind", "/sys/kernel/security", "/host/sys/kernel/security", false)}
	if ref.ID, err = docker.CreateContainer(ctx, runtime.DockerCreate{Ref: ref, Image: image, Options: options, Command: []string{"apparmor-install"}}); err != nil {
		return err
	}
	defer func() { _ = docker.RemoveContainer(context.WithoutCancel(ctx), ref) }()
	var output bytes.Buffer
	code, err := docker.StartAttached(ctx, ref, nil, &output, &output, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("loading the AppArmor profile failed: %s", strings.TrimSpace(output.String()))
	}
	if !appArmorProfileLoaded() {
		return errors.New("the AppArmor profile was written but the kernel does not list it")
	}
	return nil
}
