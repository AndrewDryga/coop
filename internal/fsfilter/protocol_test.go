package fsfilter

import (
	"slices"
	"strings"
	"testing"
)

func receiptFixtures() (string, Ready) {
	parent := "coop-fs-" + strings.Repeat("a", 32)
	boot := "01234567-89ab-cdef-0123-456789abcdef"
	ready := Ready{Status: "ready", Parent: "/" + parent, BootID: boot, CgroupInode: 1, Programs: len(hookNames), Pins: 3}
	for i, name := range hookNames {
		ready.Links = append(ready.Links, Link{Name: "guard_" + name, ID: uint32(i + 1), Program: uint32(i + 100)})
	}
	return parent, ready
}

func TestReadyRequiresCompleteDistinctAttachments(t *testing.T) {
	parent, valid := receiptFixtures()
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
