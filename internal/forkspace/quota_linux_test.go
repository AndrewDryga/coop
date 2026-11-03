//go:build linux

package forkspace

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func quotaFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Harvested from the first isolated OrbStack proof machine: a 4 GiB
	// quota behind a statfs that instead advertised the shared 2.4 TB disk.
	for name, value := range map[string]string{
		"limit_flags": "1", "max_referenced": "4294967296", "referenced": "1014648832",
		"exclusive": "1014648832", "max_exclusive": "0", "rsv_data": "0",
		"rsv_meta_pertrans": "0", "rsv_meta_prealloc": "131072",
	} {
		setQuotaValue(t, dir, name, value)
	}
	return dir
}

func setQuotaValue(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBtrfsQuotaBoundsCapacityAndPendingWrites(t *testing.T) {
	dir := quotaFixture(t)
	volume, err := constrainBtrfsQuota(dir, Filesystem{CapacityBytes: 2463432179712, FreeBytes: 2458314657792})
	if err != nil {
		t.Fatal(err)
	}
	want := Filesystem{CapacityBytes: 4294967296, FreeBytes: 4294967296 - 1014648832 - 131072}
	if volume != want {
		t.Fatalf("filesystem = %+v, want effective quota %+v", volume, want)
	}
	// A quota must never grant bytes unavailable on its physical backing disk.
	volume, err = constrainBtrfsQuota(dir, Filesystem{CapacityBytes: 2 << 30, FreeBytes: 4096})
	if err != nil || volume != (Filesystem{CapacityBytes: 2 << 30, FreeBytes: 4096}) {
		t.Fatalf("physical constraint = %+v, %v", volume, err)
	}
}

func TestBtrfsQuotaUsesTheTightestEnabledLimit(t *testing.T) {
	dir := quotaFixture(t)
	setQuotaValue(t, dir, "limit_flags", "3")
	setQuotaValue(t, dir, "max_exclusive", "2147483648")
	setQuotaValue(t, dir, "exclusive", "2147480000")
	volume, err := constrainBtrfsQuota(dir, Filesystem{CapacityBytes: 1 << 40, FreeBytes: 1 << 39})
	if err != nil || volume != (Filesystem{CapacityBytes: 2 << 30, FreeBytes: 0}) {
		t.Fatalf("exclusive constraint = %+v, %v", volume, err)
	}
	setQuotaValue(t, dir, "referenced", "9223372036854775807")
	setQuotaValue(t, dir, "rsv_data", "9223372036854644735")
	volume, err = constrainBtrfsQuota(dir, Filesystem{CapacityBytes: 1 << 40, FreeBytes: 1 << 39})
	if err != nil || volume.FreeBytes != 0 {
		t.Fatalf("overfull quota = %+v, %v", volume, err)
	}
}

func TestBtrfsQuotaCannotBecomeUnlimitedWhenCountersAreUnreadable(t *testing.T) {
	for _, value := range []string{"-1", "bad", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			dir := quotaFixture(t)
			setQuotaValue(t, dir, "referenced", value)
			if _, err := constrainBtrfsQuota(dir, Filesystem{CapacityBytes: 1 << 40, FreeBytes: 1 << 39}); err == nil {
				t.Fatal("invalid counter advertised free space")
			}
		})
	}
	dir := quotaFixture(t)
	if err := os.Remove(filepath.Join(dir, "max_referenced")); err != nil {
		t.Fatal(err)
	}
	if _, err := constrainBtrfsQuota(dir, Filesystem{CapacityBytes: 1 << 40, FreeBytes: 1 << 39}); err == nil {
		t.Fatal("missing active limit advertised free space")
	}
	setQuotaValue(t, dir, "max_referenced", "0")
	if _, err := constrainBtrfsQuota(dir, Filesystem{CapacityBytes: 1 << 40, FreeBytes: 1 << 39}); err == nil {
		t.Fatal("zero quota became unlimited")
	}
	setQuotaValue(t, dir, "max_referenced", "4294967296")
	setQuotaValue(t, dir, "rsv_data", "9223372036854775807")
	if _, err := constrainBtrfsQuota(dir, Filesystem{CapacityBytes: 1 << 40, FreeBytes: 1 << 39}); err == nil {
		t.Fatal("overflowing reservations advertised free space")
	}
}

func TestBtrfsWithoutASubvolumeLimitKeepsPhysicalSpace(t *testing.T) {
	want := Filesystem{CapacityBytes: 1 << 40, FreeBytes: 1 << 39}
	for _, directory := range []string{filepath.Join(t.TempDir(), "absent"), quotaFixture(t)} {
		if filepath.Base(directory) != "absent" {
			setQuotaValue(t, directory, "limit_flags", "0")
		}
		got, err := constrainBtrfsQuota(directory, want)
		if err != nil || got != want {
			t.Fatalf("unlimited filesystem = %+v, %v, want %+v", got, err, want)
		}
	}
}

// Explicit live qualification, never a prerequisite on an ordinary test host.
// The migration harness sets a path inside the machine's quota, including a
// capless UID1000 container's named volume, without filling or modifying it.
func TestBtrfsLiveQuotaMatchesTheConfiguredMachine(t *testing.T) {
	path := os.Getenv("COOP_TEST_BTRFS_PATH")
	if path == "" {
		t.Skip("set COOP_TEST_BTRFS_PATH for a real quota-backed filesystem")
	}
	limit, err := strconv.ParseInt(os.Getenv("COOP_TEST_BTRFS_LIMIT_BYTES"), 10, 64)
	if err != nil || limit <= 0 {
		t.Fatal("set a positive COOP_TEST_BTRFS_LIMIT_BYTES")
	}
	volume, err := MeasureFilesystem(path)
	if err != nil {
		t.Fatal(err)
	}
	if volume.CapacityBytes != limit || volume.FreeBytes < 0 || volume.FreeBytes > limit {
		t.Fatalf("quota filesystem = %+v, want capacity %d", volume, limit)
	}
	t.Logf("kernel quota capacity=%d free=%d", volume.CapacityBytes, volume.FreeBytes)
}

// Even an unexpected FIFO must not stall heartbeat storage measurement.
func TestBtrfsFilesystemIdentityNeverWaitsForAPipeWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := filesystemWithQuota(path, &syscall.Statfs_t{Type: btrfsMagic}, Filesystem{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pipe unexpectedly supplied a Btrfs identity")
		}
	case <-time.After(5 * time.Second):
		// Release a blocking old implementation before reporting the regression.
		writer, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err == nil {
			defer writer.Close()
		}
		t.Fatal("filesystem measurement waited for a pipe writer")
	}
}
