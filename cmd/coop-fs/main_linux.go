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
	if len(args) == 2 && args[0] == "unicode-table" {
		data, err := fsfilter.UnicodeData()
		if err != nil {
			return err
		}
		return os.WriteFile(args[1], data, 0444)
	}
	if handled, err := sharedCommand(args); handled {
		return err
	}
	if len(args) == 2 && (args[0] == "probe" || args[0] == "probe-warm") {
		return probeShare(args[1], args[0] == "probe-warm")
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
	if len(args) != 5 || args[0] != "admit" || !fsfilter.ValidParent(args[3]) {
		return errors.New("expected guard, healthcheck or admit")
	}
	return admit(args[1], args[2], args[3], args[4])
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
