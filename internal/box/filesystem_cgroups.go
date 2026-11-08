package box

import (
	"errors"
	"os"
	"strings"

	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/systemdbus"
)

// These reasons are sentences a person reads when a launch keeps mount-based protection.
var (
	//lint:ignore ST1005 it is a sentence for a person
	errFilesystemRootless = errors.New("Kernel filesystem protection does not work with rootless or user-namespace-remapped Docker")
	//lint:ignore ST1005 it is a sentence for a person
	errFilesystemSELinux = errors.New("Kernel filesystem protection is not yet supported with SELinux-enabled Docker")
	//lint:ignore ST1005 it is a sentence for a person
	errFilesystemCgroupV1 = errors.New("Kernel filesystem protection requires cgroup v2")
	//lint:ignore ST1005 it is a sentence for a person
	errFilesystemNoBus = errors.New("Kernel filesystem protection with Docker's systemd cgroup driver needs the system D-Bus socket at " + systemdbus.Socket)
	//lint:ignore ST1005 it is a sentence for a person
	errFilesystemCgroupDriver = errors.New("Kernel filesystem protection requires Docker's cgroupfs or systemd cgroup driver")
)

// filesystemCgroups says whether Docker can hold kernel filesystem protection, and returns
// the cgroup driver a controller records: "systemd" when the boxes' cgroups are systemd
// slices, empty for plain cgroups. Under the cgroupfs driver the helper makes plain cgroups;
// under the systemd driver it asks systemd for slices over the system bus, so that socket
// must be there. The error is the reason a launch keeps mount-based protection.
func filesystemCgroups(info runtime.DockerInfo) (driver string, err error) {
	for _, option := range info.SecurityOptions {
		if strings.Contains(option, "rootless") || strings.Contains(option, "userns") {
			return "", errFilesystemRootless
		}
		// Not yet qualified: SELinux labels may refuse the helper's bus and BPF access.
		if strings.Contains(option, "selinux") {
			return "", errFilesystemSELinux
		}
	}
	if info.CgroupVersion != "2" {
		return "", errFilesystemCgroupV1
	}
	switch info.CgroupDriver {
	case "cgroupfs":
		return "", nil
	case "systemd":
		if fi, err := os.Stat(systemdbus.Socket); err != nil || fi.Mode()&os.ModeSocket == 0 {
			return "", errFilesystemNoBus
		}
		return "systemd", nil
	}
	return "", errFilesystemCgroupDriver
}
