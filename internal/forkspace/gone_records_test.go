package forkspace

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fsidentity"
)

// goneRecordsProject makes a project at base/name and its records folder, holding a generation for
// fork "perf", its lock, and a record that names the project's own path, as candidate history does.
func goneRecordsProject(t *testing.T, base, name string) string {
	t.Helper()
	repo := filepath.Join(base, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	stored, err := fsidentity.StoredPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	writeGoneRecord(t, filepath.Join(StateDir(repo), "perf.generation.json"), `{"version":3,"name":"perf"}`)
	writeGoneRecord(t, filepath.Join(StateDir(repo), "candidate-history", "perf.1", "0000000001.json"),
		`{"canonical_root":"`+stored+`/.agent/tasks"}`)
	writeGoneRecord(t, filepath.Join(StateDir(repo), "perf.lock"), "")
	return repo
}

func writeGoneRecord(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordsKeyOf(t *testing.T, repo string) string {
	t.Helper()
	return filepath.Base(StateDir(repo))
}

// Only records that provably belong to a deleted project, or folders that hold no file at all, are
// found. A project whose folder still holds other work and is on the same disk as the folder above
// it is gone; any doubt keeps the records: a project that exists, one whose forks folder is still
// there, one whose parent folder is missing (a disk not connected, or a deleted scratch folder) or
// empty (a mount point without its disk), a folder whose project no record names, and an empty
// folder made moments ago. The removal itself keeps a project recreated meanwhile and one whose
// lock a process still holds.
func TestGoneForkRecordsRemovesOnlyRecordsNothingCanUse(t *testing.T) {
	now := time.Now()
	base := t.TempDir()
	writeGoneRecord(t, filepath.Join(base, "other-work.txt"), "kept\n")
	gone := goneRecordsProject(t, base, "gone")
	exists := goneRecordsProject(t, base, "exists")
	forksLeft := goneRecordsProject(t, base, "forks-left")
	if err := os.MkdirAll(Home(forksLeft), 0o755); err != nil {
		t.Fatal(err)
	}
	// A fork's workspace path names its project through its forks folder.
	viaWorkspace := goneRecordsProject(t, base, "workspace")
	stored, _ := fsidentity.StoredPath(viaWorkspace)
	if err := os.Remove(filepath.Join(StateDir(viaWorkspace), "candidate-history", "perf.1", "0000000001.json")); err != nil {
		t.Fatal(err)
	}
	writeGoneRecord(t, filepath.Join(StateDir(viaWorkspace), "executions", "1.json"), `{"workspace":"`+Workspace(stored, "perf")+`"}`)
	unnamed := goneRecordsProject(t, base, "unnamed")
	if err := os.RemoveAll(filepath.Join(StateDir(unnamed), "candidate-history")); err != nil {
		t.Fatal(err)
	}
	locked := goneRecordsProject(t, base, "locked")
	noParent := goneRecordsProject(t, filepath.Join(base, "scratch"), "project")
	emptyParent := goneRecordsProject(t, filepath.Join(base, "mountpoint"), "project")
	for _, path := range []string{gone, forksLeft, viaWorkspace, unnamed, locked, filepath.Dir(noParent), emptyParent} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	// A project on a disk that is not connected is missing only for now.
	unmounted := "/Volumes/coop-test-not-mounted/project"
	writeGoneRecord(t, filepath.Join(filepath.Dir(StateDir(gone)), forkRecordsKeyOf(unmounted), "candidate.json"), `{"root":"`+unmounted+`/.agent/tasks"}`)
	// Folders with no file at all: one unchanged for two days, one just made.
	root, err := PrivateStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	oldEmpty := filepath.Join(root, "forks", forkRecordsKeyOf("/old/empty"))
	newEmpty := filepath.Join(root, "forks", forkRecordsKeyOf("/new/empty"))
	for _, dir := range []string{filepath.Join(oldEmpty, "executions"), newEmpty} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-48 * time.Hour)
	for _, dir := range []string{filepath.Join(oldEmpty, "executions"), oldEmpty} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}

	found, err := GoneForkRecords(now)
	if err != nil {
		t.Fatal(err)
	}
	// Other tests share the private state root, so only this test's own folders are judged.
	mine := map[string]string{}
	for repo, label := range map[string]string{gone: "gone", exists: "exists", forksLeft: "forks-left", viaWorkspace: "workspace",
		unnamed: "unnamed", locked: "locked", noParent: "no-parent", emptyParent: "empty-parent"} {
		mine[recordsKeyOf(t, repo)] = label
	}
	for key, label := range map[string]string{forkRecordsKeyOf(unmounted): "unmounted", filepath.Base(oldEmpty): "old-empty", filepath.Base(newEmpty): "new-empty"} {
		mine[key] = label
	}
	got := map[string]string{}
	var ours GoneRecords
	for _, project := range found.Projects {
		if label, ok := mine[project.Key]; ok {
			got[label] = "gone"
			ours.Projects = append(ours.Projects, project)
			if !slices.Equal(project.Forks, []string{"perf"}) {
				t.Errorf("%s: forks %q, want perf", project.Path, project.Forks)
			}
		}
	}
	for _, key := range found.Empty {
		if label, ok := mine[key]; ok {
			got[label] = "empty"
			ours.Empty = append(ours.Empty, key)
		}
	}
	want := map[string]string{"gone": "gone", "workspace": "gone", "locked": "gone", "old-empty": "empty"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("found %v, want %v and every other folder kept", got, want)
	}

	// A project recreated before the removal keeps its records, and so does one whose lock a
	// process holds (a loop whose repository was renamed while it ran).
	if err := os.MkdirAll(viaWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Open(filepath.Join(StateDir(locked), "perf.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveGoneForkRecords(ours, now)
	if err != nil || removed != 2 {
		t.Fatalf("removed %d, %v; want the gone project and the old empty folder", removed, err)
	}
	for path, want := range map[string]bool{
		StateDir(gone): false, oldEmpty: false,
		StateDir(viaWorkspace): true, StateDir(exists): true, StateDir(forksLeft): true, StateDir(unnamed): true,
		StateDir(locked): true, StateDir(noParent): true, StateDir(emptyParent): true,
		newEmpty: true, filepath.Join(root, "forks", forkRecordsKeyOf(unmounted)): true,
	} {
		if _, err := os.Lstat(path); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", path, err == nil, want)
		}
	}
}

// An empty folder is removed deepest first with a plain rmdir, so a lock file a coop command makes
// after the check stops the removal there and survives, where RemoveAll would have deleted it.
func TestRemoveEmptyFolderKeepsAFileThatAppears(t *testing.T) {
	root, err := PrivateStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	key := forkRecordsKeyOf("/appearing/lock")
	folder := filepath.Join(root, "forks", key)
	if err := os.MkdirAll(filepath.Join(folder, "executions"), 0o700); err != nil {
		t.Fatal(err)
	}
	forks, err := openForksFolder()
	if err != nil {
		t.Fatal(err)
	}
	defer forks.Close()
	writeGoneRecord(t, filepath.Join(folder, "perf.lock"), "") // made after the folder was judged empty
	if removeEmptyFolder(forks, key) {
		t.Fatal("a folder that gained a lock file was removed")
	}
	if _, err := os.Lstat(filepath.Join(folder, "perf.lock")); err != nil {
		t.Fatalf("the new lock file is gone: %v", err)
	}
	if err := os.Remove(filepath.Join(folder, "perf.lock")); err != nil {
		t.Fatal(err)
	}
	if !removeEmptyFolder(forks, key) {
		t.Fatal("an empty folder was kept")
	}
	if _, err := os.Lstat(folder); !os.IsNotExist(err) {
		t.Fatalf("the empty folder is still there: %v", err)
	}
}
