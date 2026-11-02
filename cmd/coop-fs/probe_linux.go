//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/AndrewDryga/coop/internal/fsfilter"
)

// probeShare touches only inert files made for this admission. It runs in the
// future workload's cgroup against fresh binds of the actual host shares.
func probeShare(file string) error {
	var probe fsfilter.ShareProbe
	if err := decode(file, &probe); err != nil {
		return err
	}
	if len(probe.Denied) == 0 || len(probe.Readable) == 0 || len(probe.Denied)+len(probe.Readable) > 1024 {
		return errors.New("invalid filesystem qualification")
	}
	for _, path := range probe.Denied {
		f, err := os.Open(path)
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) {
			return fmt.Errorf("filesystem denial check failed for %q: %v", path, err)
		}
	}
	for _, path := range probe.Readable {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != fsfilter.ProbeContent {
			return fmt.Errorf("filesystem public check failed for %q", path)
		}
	}
	fmt.Println("filesystem share checks passed")
	return nil
}
