package fsfilter

import (
	"encoding/json"
	"strings"
	"testing"
)

func controllerReceipt() ControllerReady {
	b := ControllerBirth{Version: 1, Root: "coop-fs-controller-" + strings.Repeat("c", 32), BootID: "01234567-89ab-cdef-0123-456789abcdef", CgroupInode: 10, CgroupDevice: 2, PinDirectoryInode: 20, PinDirectoryDevice: 3}
	for i, name := range []string{"scope", "dispatch", "owners", "runes", "work", "objects", "paths", "devices", "config", "objects_template", "paths_template", "devices_template", "config_template"} {
		b.Maps = append(b.Maps, KernelObject{Name: name, ID: uint32(i + 1)})
	}
	b.Programs = []KernelObject{{Name: "admit_owner", ID: 100}, {Name: "hierarchy_root", ID: 101}, {Name: "release_owner", ID: 102}}
	r := ControllerReady{Status: "controller-ready"}
	for i, name := range hookNames {
		p := KernelObject{Name: "guard_" + name, ID: uint32(i + 103)}
		b.Programs = append(b.Programs, p)
		r.Links = append(r.Links, Link{Name: p.Name, ID: uint32(i + 200), Program: p.ID})
	}
	r.Birth = b
	return r
}
func TestControllerReceiptRejectsIncompleteAndForeignIdentities(t *testing.T) {
	valid := controllerReceipt()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ControllerReady){
		"root":              func(r *ControllerReady) { r.Birth.Root = "coop-fs-" + strings.Repeat("a", 32) },
		"boot":              func(r *ControllerReady) { r.Birth.BootID = "unknown" },
		"directory":         func(r *ControllerReady) { r.Birth.PinDirectoryInode = 0 },
		"dispatch missing":  func(r *ControllerReady) { r.Birth.Maps = r.Birth.Maps[1:] },
		"duplicate map":     func(r *ControllerReady) { r.Birth.Maps[0] = r.Birth.Maps[1] },
		"same map id":       func(r *ControllerReady) { r.Birth.Maps[0].ID = r.Birth.Maps[1].ID },
		"foreign program":   func(r *ControllerReady) { r.Birth.Programs[0].Name = "guard_unknown" },
		"missing link":      func(r *ControllerReady) { r.Links = r.Links[1:] },
		"wrong link target": func(r *ControllerReady) { r.Links[0].Program = r.Links[1].Program },
		"duplicate link":    func(r *ControllerReady) { r.Links[0].ID = r.Links[1].ID },
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(valid)
			var r ControllerReady
			if err := json.Unmarshal(data, &r); err != nil {
				t.Fatal(err)
			}
			change(&r)
			if r.Validate() == nil {
				t.Fatal("accepted conflicting custody")
			}
		})
	}
}
func TestSharedAdmissionBindsControllerAndFreshMaps(t *testing.T) {
	parent := "coop-fs-" + strings.Repeat("a", 32)
	c := controllerReceipt()
	b := RunBirth{Version: 2, Parent: parent, Controller: c.Birth.Root, BootID: c.Birth.BootID, CgroupInode: 30, CgroupDevice: 2, Token: 40}
	for i, name := range []string{"objects", "paths", "devices", "config"} {
		b.Maps = append(b.Maps, KernelObject{Name: name, ID: uint32(i + 300)})
	}
	if err := b.Validate(parent, c.Birth.Root); err != nil {
		t.Fatal(err)
	}
	ready := Ready{Status: "ready", Parent: "/" + parent, Controller: b.Controller, BootID: b.BootID, CgroupInode: b.CgroupInode, Token: b.Token, Pins: 3, Maps: b.Maps}
	if err := ready.Validate(parent); err != nil {
		t.Fatal(err)
	}
	if b.Validate(parent, "coop-fs-controller-"+strings.Repeat("d", 32)) == nil {
		t.Fatal("accepted another controller")
	}
	for name, change := range map[string]func(*RunBirth){
		"version": func(b *RunBirth) { b.Version = 1 }, "owner token": func(b *RunBirth) { b.Token = 0 },
		"missing map": func(b *RunBirth) { b.Maps = b.Maps[1:] }, "duplicate map": func(b *RunBirth) { b.Maps[0] = b.Maps[1] },
		"root inode": func(b *RunBirth) { b.CgroupInode = 0 }, "foreign parent": func(b *RunBirth) { b.Parent = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(b)
			var changed RunBirth
			if err := json.Unmarshal(data, &changed); err != nil {
				t.Fatal(err)
			}
			change(&changed)
			if changed.Validate(parent, c.Birth.Root) == nil {
				t.Fatal("accepted invalid admission")
			}
		})
	}
	ready.Programs = 27
	if ready.Validate(parent) == nil {
		t.Fatal("accepted mixed old/shared receipt")
	}
}

// A record 13.0.0 wrote attached the first 27 hooks. It must still read, or no launch could pass the
// controller scan and recovery could never retire it with its own image. A generation no release
// attached is still refused.
func TestEarlierHookGenerationsStillRead(t *testing.T) {
	trim := func(n int) ControllerReady {
		r := controllerReceipt()
		r.Birth.Programs = r.Birth.Programs[:3+n]
		r.Links = r.Links[:n]
		return r
	}
	if err := trim(27).Validate(); err != nil {
		t.Fatalf("13.0.0 controller record: %v", err)
	}
	if trim(28).Validate() == nil {
		t.Fatal("accepted a hook set no release attached")
	}
	parent, ready := receiptFixtures()
	ready.Links, ready.Programs = ready.Links[:27], 27
	if err := ready.Validate(parent); err != nil {
		t.Fatalf("13.0.0 admission receipt: %v", err)
	}
	ready.Links, ready.Programs = ready.Links[:26], 26
	if ready.Validate(parent) == nil {
		t.Fatal("accepted an admission receipt no release wrote")
	}
}
