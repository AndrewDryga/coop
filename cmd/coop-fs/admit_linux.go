//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
)

// C holds fresh inode pins and publishes only this admission. The shared
// controller already denies its subtree until the final owner publication.
func admit(sources, policyPath, parent, controllerPath string) error {
	id, err := currentBoot()
	if err != nil {
		return err
	}
	var controller fsfilter.ControllerReady
	if err := decode(controllerPath, &controller); err != nil {
		return err
	}
	if err := controller.Validate(); err != nil {
		return err
	}
	if controller.Birth.BootID != id {
		return errors.New("filesystem controller belongs to another kernel")
	}
	token, err := admissionToken()
	if err != nil {
		return err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return err
	}
	args := []string{"owner", sources, parent, id, strconv.FormatUint(token, 10)}
	args = append(args, controllerArguments(controller.Birth, controller.Links)...)
	command := exec.Command("/usr/local/libexec/coop-fs-kernel", args...)
	command.Stdin, command.Stdout, command.Stderr = read, os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return errors.Join(err, read.Close(), write.Close())
	}
	read.Close()
	done := make(chan struct{})
	var childErr error
	go func() {
		childErr = command.Wait()
		close(done)
	}()
	err = publishPolicy(write, policyPath, done)
	err = errors.Join(err, write.Close())
	if err != nil {
		_ = command.Process.Kill()
	}
	<-done
	return errors.Join(err, childErr)
}

func publishPolicy(pipe *os.File, path string, childDone <-chan struct{}) error {
	// This bounds only publication, never the admitted owner's lifetime.
	timeout := time.NewTimer(60 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-timeout.C:
			return errors.New("filesystem policy was not published")
		case <-childDone:
			return errors.New("filesystem loader stopped before policy publication")
		case <-ticker.C:
		}
	}
	var policy fsfilter.Admission
	if err := decode(path, &policy); err != nil {
		return err
	}
	unicodeData, err := os.ReadFile("/usr/share/coop-fs/unicode.bin")
	if err != nil {
		return err
	}
	return fsfilter.WriteKernelPolicies(pipe, policy.Roots, unicodeData)
}
