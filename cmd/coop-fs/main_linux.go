//go:build linux

// coop-fs is a release-owned helper. Its admission mode runs outside workload
// cgroups; guard mode has no privileges and executes only the frozen command.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"golang.org/x/sys/unix"
)

func decode(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing control data")
	}
	return nil
}

func currentBoot() (string, error) {
	fd, err := unix.Open("/proc/sys/kernel/random/boot_id", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), "boot_id")
	defer file.Close()
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return "", err
	}
	if st.Type != unix.PROC_SUPER_MAGIC {
		return "", errors.New("kernel boot identity is not procfs")
	}
	data, err := io.ReadAll(io.LimitReader(file, 64))
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !fsfilter.ValidBootID(id) {
		return "", errors.New("invalid kernel boot identity")
	}
	return id, nil
}

func run(args []string) error {
	const loader = "/usr/local/libexec/coop-fs-kernel"
	if len(args) == 2 && args[0] == "abandon" && fsfilter.ValidParent(args[1]) {
		// A helper which never published birth attached nothing. rmdir alone
		// refuses an unexpected surviving pin; no unknown program is detached.
		return syscall.Exec(loader, []string{loader, "release", "/host-cgroup/" + args[1], "0", "0", "0", "0"}, os.Environ())
	}
	if len(args) == 2 && args[0] == "release" {
		var birth fsfilter.Birth
		if err := decode(args[1], &birth); err != nil {
			return err
		}
		parent := strings.TrimPrefix(birth.Parent, "/")
		if err := birth.Validate(parent); err != nil {
			return err
		}
		current, err := currentBoot()
		if err != nil {
			return err
		}
		if current != birth.BootID {
			// A new kernel has none of the old attachments. Never apply old
			// numeric identities to objects created during the new boot.
			return nil
		}
		command := []string{loader, "release", "/host-cgroup/" + parent, strconv.FormatUint(birth.CgroupInode, 10), strconv.FormatUint(birth.CgroupDevice, 10), strconv.FormatUint(birth.PinDirectoryInode, 10), strconv.FormatUint(birth.PinDirectoryDevice, 10)}
		for _, link := range birth.Programs {
			command = append(command, fmt.Sprintf("%s:%d:%d", link.Name, link.ID, link.Program))
		}
		return syscall.Exec(loader, command, os.Environ())
	}
	if len(args) == 2 && args[0] == "probe" {
		return probeShare(args[1])
	}
	if len(args) == 2 && (args[0] == "guard" || args[0] == "healthcheck") {
		if args[1] != fsfilter.ControlPath+"/manifest.json" {
			return errors.New("invalid filesystem guard control path")
		}
		// A malicious image may precreate a symlink at the mount destination.
		// Refuse it before image code runs; a real read-only root-level mount
		// cannot subsequently be renamed or replaced by an unprivileged workload.
		fd, err := unix.Open(fsfilter.ControlPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		var control unix.Statfs_t
		err = unix.Fstatfs(fd, &control)
		unix.Close(fd)
		if err != nil {
			return err
		}
		if control.Flags&unix.ST_RDONLY == 0 {
			return errors.New("filesystem guard control is writable")
		}
		var manifest fsfilter.ExecutionManifest
		if err := decode(args[1], &manifest); err != nil {
			return err
		}
		current, err := currentBoot()
		if err != nil {
			return err
		}
		if !fsfilter.ValidBootID(manifest.BootID) || manifest.BootID != current {
			return errors.New("filesystem protection needs re-admission after a runtime restart")
		}
		command := manifest.Command
		if args[0] == "healthcheck" {
			command = manifest.Healthcheck
		}
		if len(command) == 0 || command[0] == "" {
			return errors.New("missing admitted command")
		}
		executable, err := exec.LookPath(command[0])
		if err != nil {
			return err
		}
		return syscall.Exec(executable, command, os.Environ())
	}
	if len(args) != 4 || args[0] != "admit" {
		return errors.New("expected guard, healthcheck or admit")
	}
	parent := args[3]
	if filepath.Dir(parent) != "/host-cgroup" || !fsfilter.ValidParent(filepath.Base(parent)) {
		return errors.New("invalid owned cgroup parent")
	}
	var policy fsfilter.Admission
	if err := decode(args[2], &policy); err != nil {
		return err
	}
	id, err := currentBoot()
	if err != nil {
		return err
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		return err
	}
	wire, err := os.CreateTemp("/tmp", "coop-fs-policy-")
	if err != nil {
		return errors.Join(err, os.Remove(parent))
	}
	writeErr := fsfilter.WriteKernelPolicies(wire, policy.Roots)
	closeErr := wire.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr, os.Remove(wire.Name()), os.Remove(parent))
	}
	err = syscall.Exec(loader, []string{loader, args[1], wire.Name(), parent, id}, os.Environ())
	return errors.Join(err, os.Remove(wire.Name()), os.Remove(parent))
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
