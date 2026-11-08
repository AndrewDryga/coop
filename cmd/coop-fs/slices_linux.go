//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/systemdbus"
)

// systemdSlices reports whether the host put this helper on systemd slices.
func systemdSlices() bool {
	key, value, _ := strings.Cut(fsfilter.SystemdCgroups, "=")
	return os.Getenv(key) == value
}

// startSlice has systemd create a slice and waits until its cgroup exists, so
// the loader attaches to a cgroup systemd owns before any box can join it. A
// name systemd already knows fails (UnitExists): like mkdir's EEXIST before, a
// slice is only ever fresh, and names are random, so recovery abandons, never reuses.
func startSlice(name, path string) error {
	bus, err := systemdbus.Dial(systemdbus.Socket, os.Getuid())
	if err != nil {
		return fmt.Errorf("systemd slice %s: %w", name, err)
	}
	defer bus.Close()
	if err := bus.StartSlice(name, "Coop kernel filesystem protection"); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := os.Lstat("/host-cgroup/" + path)
		if err == nil && info.IsDir() {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("systemd did not create slice %s", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// retireKernel runs a one-shot loader command and then stops its slice. The
// loader removes the slice's cgroup first, which fails while anything is in
// it, so stopping the slice afterwards never stops a box.
func retireKernel(slice string, args ...string) error {
	if !systemdSlices() {
		return execKernel(args...)
	}
	command := exec.Command("/usr/local/libexec/coop-fs-kernel", args...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return err
	}
	bus, err := systemdbus.Dial(systemdbus.Socket, os.Getuid())
	if err != nil {
		return fmt.Errorf("systemd slice %s: %w", slice, err)
	}
	defer bus.Close()
	return bus.StopSlice(slice)
}
