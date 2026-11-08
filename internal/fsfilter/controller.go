package fsfilter

import (
	"errors"
	"regexp"
	"strings"
)

type KernelObject struct {
	Name string `json:"name"`
	ID   uint32 `json:"id"`
}

// ControllerBirth is synced before any attachment. Program and map pins survive
// controller death; a replacement always gets a different root and birth.
type ControllerBirth struct {
	Version            int            `json:"version"`
	Root               string         `json:"root"`
	BootID             string         `json:"boot_id"`
	CgroupInode        uint64         `json:"cgroup_inode"`
	CgroupDevice       uint64         `json:"cgroup_device"`
	PinDirectoryInode  uint64         `json:"pin_directory_inode"`
	PinDirectoryDevice uint64         `json:"pin_directory_device"`
	Maps               []KernelObject `json:"maps"`
	Programs           []KernelObject `json:"programs"`
}

type ControllerReady struct {
	Status string          `json:"status"`
	Birth  ControllerBirth `json:"birth"`
	Links  []Link          `json:"links"`
}

// RunBirth precedes insertion into the shared tables. A missing entry is an
// interrupted/retired publication; a different surviving identity is a conflict.
type RunBirth struct {
	Version      int            `json:"version"`
	Parent       string         `json:"parent"`
	Controller   string         `json:"controller"`
	BootID       string         `json:"boot_id"`
	CgroupInode  uint64         `json:"cgroup_inode"`
	CgroupDevice uint64         `json:"cgroup_device"`
	Token        uint64         `json:"token"`
	Maps         []KernelObject `json:"maps"`
}

var controllerName = regexp.MustCompile(`^coop-fs-controller-[0-9a-f]{32}$`)

func ValidController(name string) bool { return controllerName.MatchString(name) }

// SystemdCgroups is the helper environment value that selects systemd slices.
// Docker's systemd cgroup driver accepts only a slice as a container's parent,
// so there the controller and each admission are slices systemd creates, named
// so the admission's slice sits directly inside the controller's.
const SystemdCgroups = "COOP_FS_CGROUPS=systemd"

// ControllerSlice is the controller's top-level slice. A dash in a slice name
// means nesting, so the name's dashes become underscores.
func ControllerSlice(controller string) string { return sliceStem(controller) + ".slice" }

// AdmissionSlice is an admission's slice inside its controller's slice; it is
// also the --cgroup-parent of the admission's boxes.
func AdmissionSlice(controller, parent string) string {
	return sliceStem(controller) + "-" + sliceStem(parent) + ".slice"
}

// AdmissionSlicePath is the admission slice's cgroup path below the hierarchy root.
func AdmissionSlicePath(controller, parent string) string {
	return ControllerSlice(controller) + "/" + AdmissionSlice(controller, parent)
}

func sliceStem(name string) string { return strings.ReplaceAll(name, "-", "_") }

func (b ControllerBirth) Validate() error {
	if b.Version != 1 || !ValidController(b.Root) || !ValidBootID(b.BootID) || b.CgroupInode == 0 || b.CgroupDevice == 0 || b.PinDirectoryInode == 0 || b.PinDirectoryDevice == 0 {
		return errors.New("invalid filesystem controller birth")
	}
	maps := []string{"scope", "dispatch", "owners", "runes", "work", "objects", "paths", "devices", "config", "objects_template", "paths_template", "devices_template", "config_template"}
	programs := []string{"admit_owner", "release_owner", "hierarchy_root"}
	for _, name := range hookNames {
		programs = append(programs, "guard_"+name)
	}
	if !exactKernelObjects(b.Maps, maps) || !exactKernelObjects(b.Programs, programs) {
		return errors.New("invalid filesystem controller objects")
	}
	return nil
}

func exactKernelObjects(objects []KernelObject, names []string) bool {
	if len(objects) != len(names) {
		return false
	}
	wanted := make(map[string]bool, len(names))
	ids := make(map[uint32]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	for _, object := range objects {
		if !wanted[object.Name] || object.ID == 0 || ids[object.ID] {
			return false
		}
		delete(wanted, object.Name)
		ids[object.ID] = true
	}
	return len(wanted) == 0
}

func (r ControllerReady) Validate() error {
	if r.Status != "controller-ready" {
		return errors.New("filesystem controller is not ready")
	}
	if err := r.Birth.Validate(); err != nil {
		return err
	}
	programs := map[string]uint32{}
	for _, p := range r.Birth.Programs {
		programs[p.Name] = p.ID
	}
	if len(r.Links) != len(hookNames) {
		return errors.New("incomplete shared filesystem attachments")
	}
	ids := map[uint32]bool{}
	names := map[string]bool{}
	for _, name := range hookNames {
		names["guard_"+name] = true
	}
	for _, link := range r.Links {
		if !names[link.Name] || link.ID == 0 || ids[link.ID] || link.Program != programs[link.Name] {
			return errors.New("invalid shared filesystem attachment")
		}
		delete(names, link.Name)
		ids[link.ID] = true
	}
	return nil
}

func (b RunBirth) Validate(parent, controller string) error {
	if b.Version != 2 || !ValidParent(parent) || !ValidController(controller) || b.Parent != parent || b.Controller != controller || !ValidBootID(b.BootID) || b.CgroupInode == 0 || b.CgroupDevice == 0 || b.Token == 0 || !exactKernelObjects(b.Maps, []string{"objects", "paths", "devices", "config"}) {
		return errors.New("invalid shared filesystem admission birth")
	}
	return nil
}
