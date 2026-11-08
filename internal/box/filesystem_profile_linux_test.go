package box

import (
	"os"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFilesystemProfileCandidateIsExt4OrXfs(t *testing.T) {
	dir := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	want := fs.Type == unix.EXT4_SUPER_MAGIC || fs.Type == unix.XFS_SUPER_MAGIC
	if filesystemProfileCandidate(dir) != want {
		t.Fatalf("candidate(%s) on filesystem %#x = %v", dir, fs.Type, !want)
	}
	if filesystemProfileCandidate("/proc") || filesystemProfileCandidate(dir+"/missing") {
		t.Fatal("procfs or a missing folder qualified")
	}
}

// The probe leaves nothing behind and never errors where casefolding is unsupported.
func TestFilesystemMayFoldLeavesNoTrace(t *testing.T) {
	dir := t.TempDir()
	if _, err := filesystemMayFold(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("probe left %v", entries)
	}
}

func TestFilesystemKernelReadyFollowsTheSecurityModules(t *testing.T) {
	data, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil {
		if filesystemKernelReady() == nil {
			t.Fatal("ready without a readable module list")
		}
		return
	}
	on := slices.Contains(strings.Split(strings.TrimSpace(string(data)), ","), "bpf")
	if err := filesystemKernelReady(); (err == nil) != on {
		t.Fatalf("modules %q: %v", data, err)
	}
}
