//go:build linux

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"

	"github.com/AndrewDryga/coop/internal/fsfilter"
)

func decodeControl(data string, value any) error {
	decoder := json.NewDecoder(bytes.NewBufferString(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("unexpected trailing control data")
	}
	return nil
}

func controllerArguments(b fsfilter.ControllerBirth, links []fsfilter.Link) []string {
	args := []string{b.Root, strconv.FormatUint(b.CgroupInode, 10), strconv.FormatUint(b.CgroupDevice, 10), strconv.FormatUint(b.PinDirectoryInode, 10), strconv.FormatUint(b.PinDirectoryDevice, 10)}
	for _, m := range b.Maps {
		args = append(args, fmt.Sprintf("m:%s:%d", m.Name, m.ID))
	}
	for _, p := range b.Programs {
		args = append(args, fmt.Sprintf("p:%s:%d", p.Name, p.ID))
	}
	for _, l := range links {
		args = append(args, fmt.Sprintf("l:%s:%d", l.Name, l.ID))
	}
	return args
}

const kernelLoader = "/usr/local/libexec/coop-fs-kernel"

func execKernel(args ...string) error {
	return syscall.Exec(kernelLoader, append([]string{kernelLoader}, args...), os.Environ())
}

func retireRun(b fsfilter.RunBirth, controller fsfilter.ControllerReady) error {
	if err := controller.Validate(); err != nil {
		return err
	}
	if err := b.Validate(b.Parent, controller.Birth.Root); err != nil {
		return err
	}
	if b.BootID != controller.Birth.BootID {
		return errors.New("filesystem birth belongs to another kernel")
	}
	current, err := currentBoot()
	if err != nil {
		return err
	}
	if current != b.BootID {
		return nil
	}
	args := []string{"release-run", b.Parent, strconv.FormatUint(b.CgroupInode, 10), strconv.FormatUint(b.CgroupDevice, 10), strconv.FormatUint(b.Token, 10)}
	for _, name := range []string{"objects", "paths", "devices", "config"} {
		for _, m := range b.Maps {
			if m.Name == name {
				args = append(args, strconv.FormatUint(uint64(m.ID), 10))
			}
		}
	}
	return retireKernel(fsfilter.AdmissionSlice(b.Controller, b.Parent), append(args, controllerArguments(controller.Birth, controller.Links)...)...)
}

func sharedCommand(args []string) (bool, error) {
	if len(args) == 2 && args[0] == "controller" && fsfilter.ValidController(args[1]) {
		boot, err := currentBoot()
		if err != nil {
			return true, err
		}
		if systemdSlices() {
			if err := startSlice(fsfilter.ControllerSlice(args[1]), fsfilter.ControllerSlice(args[1])); err != nil {
				return true, err
			}
		}
		return true, execKernel("controller", args[1], boot)
	}
	if (len(args) == 2 || len(args) == 3) && args[0] == "abandon" && fsfilter.ValidController(args[1]) {
		if len(args) == 3 && !fsfilter.ValidParent(args[2]) {
			return true, errors.New("invalid admission name")
		}
		slice := fsfilter.ControllerSlice(args[1])
		if len(args) == 3 {
			slice = fsfilter.AdmissionSlice(args[1], args[2])
		}
		return true, retireKernel(slice, args...)
	}
	if len(args) == 3 && (args[0] == "release-run" || args[0] == "retire-run") {
		var birth fsfilter.RunBirth
		var controller fsfilter.ControllerReady
		var err error
		if args[0] == "release-run" {
			err = decode(args[1], &birth)
			if err == nil {
				err = decode(args[2], &controller)
			}
		} else {
			err = decodeControl(args[1], &birth)
			if err == nil {
				err = decodeControl(args[2], &controller)
			}
		}
		if err != nil {
			return true, err
		}
		return true, retireRun(birth, controller)
	}
	if len(args) == 3 && args[0] == "release-controller" {
		var birth fsfilter.ControllerBirth
		if err := decode(args[1], &birth); err != nil {
			return true, err
		}
		if err := birth.Validate(); err != nil {
			return true, err
		}
		var ready fsfilter.ControllerReady
		var links []fsfilter.Link
		if err := decode(args[2], &ready); err == nil {
			if err := ready.Validate(); err != nil {
				return true, err
			}
			a, _ := json.Marshal(birth)
			b, _ := json.Marshal(ready.Birth)
			if !bytes.Equal(a, b) {
				return true, errors.New("controller readiness differs from birth")
			}
			links = ready.Links
		} else if !errors.Is(err, os.ErrNotExist) {
			return true, err
		}
		current, err := currentBoot()
		if err != nil {
			return true, err
		}
		if current != birth.BootID {
			return true, nil
		}
		return true, retireKernel(fsfilter.ControllerSlice(birth.Root), append([]string{"release-controller"}, controllerArguments(birth, links)...)...)
	}
	return false, nil
}

func admissionToken() (uint64, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0, err
	}
	token := binary.LittleEndian.Uint64(raw[:])
	if token == 0 {
		return admissionToken()
	}
	return token, nil
}
