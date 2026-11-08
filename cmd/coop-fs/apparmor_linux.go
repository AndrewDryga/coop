//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/fsfilter"
)

// installAppArmor writes the filesystem helper's profile into the host's /etc/apparmor.d (mounted
// at /host-apparmor), unloads and removes older versions, then loads the new one with the host's
// own apparmor_parser inside the host's root (mounted read-only at /host). The parser compiles the
// profile against the host's own AppArmor abstractions, which this image does not carry.
func installAppArmor() error {
	name := fsfilter.AppArmorProfile
	temporary := filepath.Join("/host-apparmor", "."+name+".new")
	if err := os.WriteFile(temporary, []byte(fsfilter.AppArmorPolicy()), 0644); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join("/host-apparmor", name)); err != nil {
		return errors.Join(err, os.Remove(temporary))
	}
	entries, err := os.ReadDir("/host-apparmor")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		old := entry.Name()
		if old == name || !strings.HasPrefix(old, fsfilter.AppArmorProfilePrefix) {
			continue
		}
		// A profile that is not loaded has nothing to unload.
		if err := os.WriteFile("/host/sys/kernel/security/apparmor/.remove", []byte(old), 0); err != nil && !errors.Is(err, syscall.ENOENT) {
			return err
		}
		if err := os.Remove(filepath.Join("/host-apparmor", old)); err != nil {
			return err
		}
	}
	if err := syscall.Chroot("/host"); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	for _, parser := range []string{"/usr/sbin/apparmor_parser", "/sbin/apparmor_parser"} {
		if _, err := os.Stat(parser); err == nil {
			return syscall.Exec(parser, []string{parser, "-r", "/etc/apparmor.d/" + name}, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"})
		}
	}
	return errors.New("the host has no apparmor_parser; install the apparmor package")
}
