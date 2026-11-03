package box

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/runtime"
	"golang.org/x/sys/unix"
)

func TestFilesystemRetiredAdmissionIsNotReusableOrRecoverable(t *testing.T) {
	dir := t.TempDir()
	lease, err := lockFilesystemAdmission(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := lockFilesystemAdmission(dir, false); !os.IsNotExist(err) {
		t.Fatalf("retired lease: %v", err)
	}
	a, err := reuseLockedServiceFilesystem(t.Context(), runtime.Runtime{Name: "docker"}, dir, lease, "", "", nil, "")
	if a != nil || err != nil {
		t.Fatalf("retired candidate should be skipped: %v %v", a, err)
	}
	if removed, err := recoverFilesystemAdmission(t.Context(), runtime.Runtime{Name: "docker"}, dir, lease, "", ""); removed || err != nil {
		t.Fatalf("already retired candidate: %t %v", removed, err)
	}
}

func TestFilesystemLeaseExcludesRecoveryAndRejectsLinks(t *testing.T) {
	dir := t.TempDir()
	first, err := lockFilesystemAdmission(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockFilesystemAdmission(dir, false); !errors.Is(err, unix.EWOULDBLOCK) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second owner acquired lease: %v", err)
	}
	first.Close()
	second, err := lockFilesystemAdmission(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	if err := os.Remove(filepath.Join(dir, "lease")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(dir, "lease")); err != nil {
		t.Fatal(err)
	}
	if lease, err := lockFilesystemAdmission(dir, false); err == nil {
		lease.Close()
		t.Fatal("followed lease link")
	}
}

// leftoverProbe makes the probe folder a launch from a Coop version before 2026-11-03 left at
// repo when it crashed, and returns the record that version kept for it.
func leftoverProbe(t *testing.T, repo string) filesystemProbeRecord {
	t.Helper()
	dir, err := os.MkdirTemp(repo, filesystemProbePrefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", "Caf\u00e9", "public", "warm-alias"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(filesystemProbeContent), 0644); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := identityOf(info)
	return filesystemProbeRecord{Root: repo, Leaf: filepath.Base(dir), Dev: id.dev, Ino: id.ino}
}

func TestFilesystemProbeCleanupKeepsReplacedDirectories(t *testing.T) {
	repo := t.TempDir()
	saved := leftoverProbe(t, repo)
	name := filepath.Join(saved.Root, saved.Leaf)
	if err := os.Rename(name, name+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(name, "keep"), []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := saved.close(); err == nil {
		t.Fatal("removed replacement")
	}
	if _, err := os.Stat(filepath.Join(name, "keep")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(name+"-old", name); err != nil {
		t.Fatal(err)
	}
	if err := saved.close(); err != nil {
		t.Fatal(err)
	}
	if err := saved.close(); err != nil {
		t.Fatal("cleanup is not retryable", err)
	}
}

func TestFilesystemProbeCleanupValidatesInertContents(t *testing.T) {
	for _, kind := range []string{"renumbered-device", "partial-cleanup", "foreign-file", "changed-content", "symlink", "directory", "forged-inode"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			probe := leftoverProbe(t, repo)
			dir := filepath.Join(repo, probe.Leaf)
			success := false
			var err error
			switch kind {
			case "renumbered-device":
				probe.Dev++
				success = true
			case "partial-cleanup":
				err = os.Remove(filepath.Join(dir, "public"))
				success = true
			case "foreign-file":
				err = os.WriteFile(filepath.Join(dir, "keep"), []byte("foreign bytes"), 0600)
			case "changed-content":
				err = os.WriteFile(filepath.Join(dir, "public"), []byte("foreign bytes"), 0600)
			case "symlink":
				err = os.Symlink(filepath.Join(dir, "public"), filepath.Join(dir, "replacement"))
			case "directory":
				err = os.Mkdir(filepath.Join(dir, "replacement"), 0700)
			case "forged-inode":
				if err := os.Rename(dir, dir+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(dir)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := identityOf(info)
				probe.Ino = id.ino
				if err := os.WriteFile(filepath.Join(dir, "public"), []byte("foreign bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := probe.close(); (err == nil) != success {
				t.Fatalf("cleanup success=%t: %v", success, err)
			}
			_, err = os.Stat(dir)
			if success != os.IsNotExist(err) {
				t.Fatalf("directory removal success=%t: %v", success, err)
			}
			if !success {
				if _, err := os.Lstat(filepath.Join(dir, "public")); err != nil {
					t.Fatal("cleanup changed rejected inventory", err)
				}
			}
		})
	}
}

// Records from earlier versions list their probe folders; they must still decode so retirement can
// finish, while new records never mention probes.
func TestFilesystemRecordKeepsEarlierProbesRetirable(t *testing.T) {
	parent := "coop-fs-" + strings.Repeat("c", 32)
	repo := t.TempDir()
	record := filesystemRecord{Version: 1, Parent: parent, Endpoint: "unix:///runtime.sock", DaemonID: "daemon", Workspace: repo, Supervisor: supervisorLabelValue(workspaceScope(repo), os.Getpid()), Helper: filesystemRef(parent, "owner")}
	if record.Supervisor == "" {
		t.Skip("this host has no stable process identity, so no admission can be recorded")
	}
	fresh := t.TempDir()
	if err := filesystemJSON(filepath.Join(fresh, "record.json"), record); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(fresh, "record.json")); err != nil || strings.Contains(string(data), `"Probes":`) {
		t.Fatalf("a new record mentions probes: %s %v", data, err)
	}
	probe := leftoverProbe(t, repo)
	record.Probes = []filesystemProbeRecord{probe}
	earlier := t.TempDir()
	if err := filesystemJSON(filepath.Join(earlier, "record.json"), record); err != nil {
		t.Fatal(err)
	}
	read, err := readFilesystemRecord(earlier)
	if err != nil || len(read.Probes) != 1 || read.Probes[0] != probe {
		t.Fatalf("an earlier record did not decode its probe: %+v %v", read.Probes, err)
	}
	if err := read.Probes[0].close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(repo, probe.Leaf)); !os.IsNotExist(err) {
		t.Fatalf("retirement left the probe folder: %v", err)
	}
}

func TestFilesystemRecordRejectsUnownedResources(t *testing.T) {
	parent := "coop-fs-" + strings.Repeat("a", 32)
	repo := t.TempDir()
	valid := filesystemRecord{Version: 1, Parent: parent, Endpoint: "unix:///runtime.sock", DaemonID: "daemon", Workspace: repo, Supervisor: supervisorLabelValue(workspaceScope(repo), os.Getpid()), Helper: filesystemRef(parent, "owner")}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*filesystemRecord){
		func(r *filesystemRecord) { r.Parent = "../outside" },
		func(r *filesystemRecord) { r.Supervisor = "" },
		func(r *filesystemRecord) { r.Pending = "unrelated" },
		func(r *filesystemRecord) { r.Image = "mutable:tag" },
		func(r *filesystemRecord) { r.Helper = filesystemRef("other", "owner") },
		func(r *filesystemRecord) { r.Consumers = append(r.Consumers, r.Helper) },
		func(r *filesystemRecord) {
			r.Probes = []filesystemProbeRecord{{Root: repo, Leaf: "../outside", Dev: 1, Ino: 1}}
		},
	} {
		r := valid
		change(&r)
		if r.validate() == nil {
			t.Fatalf("accepted changed record %#v", r)
		}
	}
}

func TestFilesystemOverlappingRootsRefused(t *testing.T) {
	for _, roots := range [][]string{{"/repo", "/repo"}, {"/repo", "/repo/sub"}, {"/repo/sub", "/repo"}} {
		if nonoverlappingFilesystemRoots(roots) == nil {
			t.Fatalf("accepted nested roots %v", roots)
		}
	}
	if err := nonoverlappingFilesystemRoots([]string{"/repo", "/repository"}); err != nil {
		t.Fatal(err)
	}
}

// A record from an earlier version can outlive its project folder (a deleted temporary project or
// worktree). Its probe folder went with it, so retirement must not stay stuck on it.
func TestFilesystemProbeCleanupFinishesWhenTheProjectIsGone(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	probe := leftoverProbe(t, repo)
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	if err := probe.close(); err != nil {
		t.Fatalf("a probe whose project is gone kept its record: %v", err)
	}
}
