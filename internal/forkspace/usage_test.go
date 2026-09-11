package forkspace

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeBytes(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A fork's git clone hardlinks the parent's object files, so a scan that added every link's blocks
// would report storage that discarding the fork can never return. The shared half has to be
// separable from the exclusive half, because only the exclusive half is reclaimable.
func TestMeasureUsageCountsAHardlinkedInodeOnceAndNamesItShared(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "exclusive.bin"), 256<<10)
	writeBytes(t, filepath.Join(root, "objects", "shared.bin"), 256<<10)
	if err := os.Link(filepath.Join(root, "objects", "shared.bin"), filepath.Join(root, "objects", "shared.link")); err != nil {
		t.Fatal(err)
	}

	usage, err := MeasureUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Unknown {
		t.Fatalf("a readable tree measured as unknown: %+v", usage)
	}
	// Two distinct 256 KiB payloads plus directory blocks; a third link must add nothing.
	if usage.AllocatedBytes < 512<<10 || usage.AllocatedBytes > 640<<10 {
		t.Fatalf("allocated bytes = %d, want the two distinct payloads only", usage.AllocatedBytes)
	}
	if usage.SharedBytes < 256<<10 || usage.SharedBytes > 288<<10 {
		t.Fatalf("shared bytes = %d, want the multiply-linked payload", usage.SharedBytes)
	}
	if got := usage.ExclusiveBytes(); got < 256<<10 || got > 384<<10 {
		t.Fatalf("exclusive bytes = %d, want the single-link payload", got)
	}
	t.Logf("allocated=%d shared=%d exclusive=%d entries=%d", usage.AllocatedBytes, usage.SharedBytes, usage.ExclusiveBytes(), usage.Entries)
}

// Ten forks cloned from one checkout hardlink the same git objects. Measured one at a time each
// would claim the whole baseline, and the fork root's total would report storage the volume does
// not hold. One scan across the root charges a shared inode exactly once.
func TestUsageScanChargesASharedInodeToOneTreeOnly(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "fork-a")
	second := filepath.Join(root, "fork-b")
	writeBytes(t, filepath.Join(first, "objects", "pack.bin"), 256<<10)
	writeBytes(t, filepath.Join(first, "checkout.bin"), 64<<10)
	if err := os.MkdirAll(filepath.Join(second, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(second, "checkout.bin"), 64<<10)
	if err := os.Link(filepath.Join(first, "objects", "pack.bin"), filepath.Join(second, "objects", "pack.bin")); err != nil {
		t.Fatal(err)
	}

	scan := NewUsageScan()
	one, err := scan.Measure(first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := scan.Measure(second)
	if err != nil {
		t.Fatal(err)
	}
	if one.SharedBytes < 256<<10 {
		t.Fatalf("first tree shared bytes = %d, want the whole baseline", one.SharedBytes)
	}
	if two.SharedBytes != 0 {
		t.Fatalf("second tree charged %d shared bytes the scan had already counted", two.SharedBytes)
	}
	total := one.Add(two)
	if total.AllocatedBytes > 448<<10 {
		t.Fatalf("root total = %d, want the baseline counted once plus two checkouts", total.AllocatedBytes)
	}
	if one.ExclusiveBytes() < 64<<10 || two.ExclusiveBytes() < 64<<10 {
		t.Fatalf("exclusive bytes = %d and %d, want each checkout charged to its own tree",
			one.ExclusiveBytes(), two.ExclusiveBytes())
	}
}

// "Unknown is not zero." A subtree coop cannot read is storage that exists; reporting 0 for it
// would tell the control plane the disk is emptier than it is.
func TestMeasureUsageReportsUnknownForAnUnreadableSubtree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory, so the unreadable case cannot be staged")
	}
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "visible.bin"), 128<<10)
	hidden := filepath.Join(root, "hidden")
	writeBytes(t, filepath.Join(hidden, "inside.bin"), 128<<10)
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o755) })

	usage, err := MeasureUsage(root)
	if err != nil {
		t.Fatalf("an unreadable subtree must not fail the whole measurement: %v", err)
	}
	if !usage.Unknown {
		t.Fatal("an unreadable subtree measured as fully known")
	}
	if usage.AllocatedBytes < 128<<10 {
		t.Fatalf("allocated bytes = %d, want at least the readable payload", usage.AllocatedBytes)
	}
}

// A symlink out of the fork root must never charge the target's bytes to the fork, and must never
// let a scan wander into the rest of the filesystem.
func TestMeasureUsageNeverFollowsSymlinks(t *testing.T) {
	outside := t.TempDir()
	writeBytes(t, filepath.Join(outside, "foreign.bin"), 512<<10)
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "own.bin"), 64<<10)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	usage, err := MeasureUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	if usage.AllocatedBytes > 128<<10 {
		t.Fatalf("allocated bytes = %d: the symlink target was followed", usage.AllocatedBytes)
	}

	linked := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := MeasureUsage(linked); err == nil {
		t.Fatal("a symlinked measurement root was accepted")
	}
}

func TestMeasureUsageOfAMissingTreeIsNotExist(t *testing.T) {
	if _, err := MeasureUsage(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("MeasureUsage of a missing tree = %v, want ErrNotExist", err)
	}
}

// The pressure check is only as honest as the free-space reading behind it.
func TestFilesystemCapacityReportsTheVolumeHoldingThePath(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("statfs is not available on %s", runtime.GOOS)
	}
	filesystem, err := MeasureFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if filesystem.CapacityBytes <= 0 || filesystem.FreeBytes < 0 || filesystem.FreeBytes > filesystem.CapacityBytes {
		t.Fatalf("filesystem = %+v, want a positive capacity and a free value inside it", filesystem)
	}
	if _, err := MeasureFilesystem(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing path reported a filesystem")
	}
}
