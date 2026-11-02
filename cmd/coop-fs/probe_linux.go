//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
)

// probeShare touches only inert files made for this admission. It runs in the
// future workload's cgroup against fresh binds of the actual host shares.
func probeShare(file string, warm bool) error {
	var probe fsfilter.ShareProbe
	if err := decode(file, &probe); err != nil {
		return err
	}
	if (!warm && len(probe.Denied) == 0) || (warm && len(probe.Denied) != 0) || len(probe.Readable) == 0 || len(probe.Denied)+len(probe.Readable) > 1024 {
		return errors.New("invalid filesystem qualification")
	}
	missing, err := probeDenied(probe.Denied, func(path string) error {
		f, err := os.Open(path)
		if f != nil {
			f.Close()
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, path := range probe.Readable {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != fsfilter.ProbeContent {
			return fmt.Errorf("filesystem public check failed for %q", path)
		}
	}
	if missing != 0 {
		fmt.Printf("filesystem share checks passed after %d missing path observations\n", missing)
	} else {
		fmt.Println("filesystem share checks passed")
	}
	return nil
}

func probeDenied(paths []string, open func(string) error) (int, error) {
	// Host rename completion can precede guest visibility. Observe only this
	// pathname until it actually denies; another alias could refresh its cache.
	// One deadline bounds all absence waits, not each path independently.
	deadline := time.Now().Add(2 * time.Second)
	missing := 0
	for _, path := range paths {
		for {
			err := open(path)
			if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
				break
			}
			if errors.Is(err, syscall.ENOENT) {
				missing++
				if remaining := time.Until(deadline); remaining > 0 {
					time.Sleep(min(10*time.Millisecond, remaining))
					continue
				}
			}
			err = fmt.Errorf("filesystem denial check failed for %q: %v", path, err)
			if missing != 0 {
				err = fmt.Errorf("%w (after %d missing path observations)", err, missing)
			}
			return missing, err
		}
	}
	return missing, nil
}
