package fsfilter

import (
	"slices"
	"strings"
	"testing"
)

func receiptFixtures() (string, Ready, Birth) {
	parent := "coop-fs-" + strings.Repeat("a", 32)
	boot := "01234567-89ab-cdef-0123-456789abcdef"
	ready := Ready{Status: "ready", Parent: "/" + parent, BootID: boot, CgroupInode: 1, Programs: len(hookNames), Pins: 3}
	birth := Birth{Parent: ready.Parent, BootID: boot, CgroupInode: 1, CgroupDevice: 2, PinDirectoryInode: 3, PinDirectoryDevice: 4}
	for i, name := range hookNames {
		ready.Links = append(ready.Links, Link{Name: "guard_" + name, ID: uint32(i + 1), Program: uint32(i + 100)})
		birth.Programs = append(birth.Programs, Link{Name: "guard_" + name, Program: uint32(i + 100)})
	}
	return parent, ready, birth
}

func TestReadyRequiresCompleteDistinctAttachments(t *testing.T) {
	parent, valid, _ := receiptFixtures()
	if err := valid.Validate(parent); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Ready){
		func(r *Ready) { r.Parent = "/other" }, func(r *Ready) { r.BootID = "unknown" },
		func(r *Ready) { r.CgroupInode = 0 }, func(r *Ready) { r.Pins = MaxPaths + 1 },
		func(r *Ready) { r.Programs-- }, func(r *Ready) { r.Links = r.Links[1:] },
		func(r *Ready) { r.Links[0].Name = "guard_unknown" },
		func(r *Ready) { r.Links[0].Name = r.Links[1].Name },
		func(r *Ready) { r.Links[0].ID = r.Links[1].ID },
		func(r *Ready) { r.Links[0].Program = 0 },
	} {
		r := valid
		r.Links = slices.Clone(valid.Links)
		change(&r)
		if r.Validate(parent) == nil {
			t.Fatalf("accepted incomplete or foreign receipt: %#v", r)
		}
	}
}

func TestBirthReconcilesOnlyItsCompleteProgramSet(t *testing.T) {
	parent, _, valid := receiptFixtures()
	if err := valid.Validate(parent); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Birth){
		func(b *Birth) { b.Parent = "/other" }, func(b *Birth) { b.BootID = "unknown" },
		func(b *Birth) { b.CgroupInode = 0 }, func(b *Birth) { b.CgroupDevice = 0 },
		func(b *Birth) { b.PinDirectoryInode = 0 }, func(b *Birth) { b.PinDirectoryDevice = 0 },
		func(b *Birth) { b.Programs = b.Programs[1:] },
		func(b *Birth) { b.Programs[0].Name = b.Programs[1].Name },
		func(b *Birth) { b.Programs[0].Program = 0 }, func(b *Birth) { b.Programs[0].ID = 1 },
	} {
		b := valid
		b.Programs = slices.Clone(valid.Programs)
		change(&b)
		if b.Validate(parent) == nil {
			t.Fatalf("accepted incomplete or foreign birth: %#v", b)
		}
	}
}
