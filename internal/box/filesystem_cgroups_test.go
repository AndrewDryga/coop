package box

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/systemdbus"
)

func TestFilesystemCgroupsNamesWhyAHostKeepsMounts(t *testing.T) {
	for _, tc := range []struct {
		info   runtime.DockerInfo
		reason string
	}{
		{runtime.DockerInfo{CgroupVersion: "2", CgroupDriver: "cgroupfs", SecurityOptions: []string{"name=rootless"}}, "rootless"},
		{runtime.DockerInfo{CgroupVersion: "2", CgroupDriver: "cgroupfs", SecurityOptions: []string{"name=userns"}}, "rootless or user-namespace"},
		{runtime.DockerInfo{CgroupVersion: "1", CgroupDriver: "cgroupfs"}, "cgroup v2"},
		{runtime.DockerInfo{CgroupVersion: "2", CgroupDriver: "none"}, "cgroupfs or systemd"},
		{runtime.DockerInfo{}, "cgroup v2"},
	} {
		if _, err := filesystemCgroups(tc.info); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%+v: %v, want a reason naming %q", tc.info, err, tc.reason)
		}
	}
	if driver, err := filesystemCgroups(runtime.DockerInfo{CgroupVersion: "2", CgroupDriver: "cgroupfs"}); driver != "" || err != nil {
		t.Errorf("cgroupfs = %q, %v", driver, err)
	}
	// The systemd driver needs this host's system bus, which only some test hosts have.
	driver, err := filesystemCgroups(runtime.DockerInfo{CgroupVersion: "2", CgroupDriver: "systemd"})
	if info, statErr := os.Stat(systemdbus.Socket); statErr == nil && info.Mode()&os.ModeSocket != 0 {
		if driver != "systemd" || err != nil {
			t.Errorf("systemd with a bus = %q, %v", driver, err)
		}
	} else if err == nil || !strings.Contains(err.Error(), systemdbus.Socket) {
		t.Errorf("systemd without a bus = %q, %v", driver, err)
	}
}

// On systemd slices the helper gets the bus, the slice layout and no AppArmor profile, and an
// admission's boxes take its slice as their parent; plain cgroups keep the old shape.
func TestFilesystemSystemdSlicesShapeTheHelperAndParent(t *testing.T) {
	a := &filesystemAdmission{dir: "/state"}
	plain, sliced := a.helperOptions(false, false), a.helperOptions(false, true)
	if slices.Contains(plain, fsfilter.SystemdCgroups) || slices.Contains(plain, "apparmor=unconfined") || strings.Contains(strings.Join(plain, " "), systemdbus.Socket) {
		t.Fatalf("plain cgroups got systemd options: %v", plain)
	}
	if !slices.Contains(plain, "DAC_OVERRIDE") || !slices.Equal(sliced[:len(plain)], plain) {
		t.Fatalf("helper privileges changed shape: %v", sliced)
	}
	extra := strings.Join(sliced[len(plain):], " ")
	if extra != "--security-opt apparmor=unconfined -e "+fsfilter.SystemdCgroups+" --mount "+networkMount("bind", systemdbus.Socket, systemdbus.Socket, false) {
		t.Fatalf("systemd options = %q", extra)
	}

	parent := "coop-fs-" + strings.Repeat("a", 32)
	controller := "coop-fs-controller-" + strings.Repeat("b", 32)
	r := filesystemRecord{Parent: parent, Controller: &filesystemControllerRecord{Name: controller, CgroupDriver: "systemd"}}
	want := "coop_fs_controller_" + strings.Repeat("b", 32) + "-coop_fs_" + strings.Repeat("a", 32) + ".slice"
	if r.cgroupParent() != want || r.Controller.cgroupParent() != "coop_fs_controller_"+strings.Repeat("b", 32)+".slice" {
		t.Fatalf("slice parents = %q, %q", r.cgroupParent(), r.Controller.cgroupParent())
	}
	if fsfilter.AdmissionSlicePath(controller, parent) != r.Controller.cgroupParent()+"/"+want {
		t.Fatalf("admission slice path = %q", fsfilter.AdmissionSlicePath(controller, parent))
	}
	r.Controller.CgroupDriver = ""
	if r.cgroupParent() != "/"+controller+"/"+parent || r.Controller.cgroupParent() != "/"+controller {
		t.Fatal("plain cgroups changed their parents")
	}
}

// A record says which layout its cgroups use; older records have none and are plain cgroups.
func TestFilesystemControllerRecordCgroupDriver(t *testing.T) {
	r := readyControllerRecord(t, "coop-fs-controller-"+strings.Repeat("c", 32), "unix:///var/run/docker.sock")
	for driver, valid := range map[string]bool{"": true, "systemd": true, "cgroupfs": false, "Systemd": false} {
		r.CgroupDriver = driver
		if err := r.validate(); (err == nil) != valid {
			t.Errorf("driver %q: %v", driver, err)
		}
	}
}
