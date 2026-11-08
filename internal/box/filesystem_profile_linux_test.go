package box

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/fsfilter"
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
	ready := slices.Contains(strings.Split(strings.TrimSpace(string(data)), ","), "bpf")
	btf, btfErr := os.ReadFile("/sys/kernel/btf/vmlinux")
	ready = ready && btfErr == nil
	for _, hook := range fsfilter.HookNames() {
		ready = ready && bytes.Contains(btf, []byte("\x00bpf_lsm_"+hook+"\x00"))
	}
	if err := filesystemKernelReady(); (err == nil) != ready {
		t.Fatalf("modules %q, all hooks %v: %v", data, ready, err)
	}
}
