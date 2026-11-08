package runtime

import (
	"os"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
)

// selinuxLabel is how every container Coop creates runs on an SELinux-enabled Docker. Docker would
// otherwise label it container_t, which may not read a project, agent home or credential folder
// labelled as the user's files, so a box could not even read README.md. Relabeling them (:z)
// would rewrite the labels of the user's own files on every start; toolbox, distrobox and dev
// containers make the same choice. Capabilities, seccomp, the user, mounts and filesystem
// protection still confine the box; SELinux type enforcement does not.
const selinuxLabel = "--security-opt=label=disable"

func selinuxDaemon(options []string) bool {
	return slices.ContainsFunc(options, func(o string) bool { return strings.Contains(o, "name=selinux") })
}

// withSELinuxLabel adds selinuxLabel after the run or create subcommand of a Docker argv.
func withSELinuxLabel(selinux bool, args []string) []string {
	if !selinux {
		return args
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "--host", "-H", "--context", "-c", "--log-level", "-l":
			i++ // a global option and its value
		case "container":
			if i+1 < len(args) && (args[i+1] == "run" || args[i+1] == "create") {
				return slices.Insert(slices.Clone(args), i+2, selinuxLabel)
			}
			return args
		case "run", "create":
			return slices.Insert(slices.Clone(args), i+1, selinuxLabel)
		default:
			if !strings.HasPrefix(args[i], "-") {
				return args // any other subcommand
			}
		}
	}
	return args
}

// selinuxFor reports whether an argv creates a container on a host that runs SELinux. SELinux is a
// property of the Linux host Docker runs on, so the host's own selinuxfs answers without asking
// Docker. Docker without its SELinux support ignores the label option.
func (r Runtime) selinuxFor(args []string) bool {
	return r.kind() == runtimeDocker && len(withSELinuxLabel(true, args)) != len(args) && hostSELinux()
}

// HostSELinux reports whether this Linux host runs SELinux. Compose services Coop defines take the
// same label option itself (SELinuxComposeOption), since Compose creates their containers.
func HostSELinux() bool { return hostSELinux() }

// SELinuxComposeOption is selinuxLabel as a Compose security_opt entry.
const SELinuxComposeOption = "label=disable"

var hostSELinux = sync.OnceValue(func() bool {
	if goruntime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat("/sys/fs/selinux/enforce")
	return err == nil
})
