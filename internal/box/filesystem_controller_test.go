package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/runtime"
	"golang.org/x/sys/unix"
)

func TestFilesystemControllerEnrollmentExcludesRetirement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease")
	first, err := filesystemControllerLock(t.Context(), path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := filesystemControllerLock(t.Context(), path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if ex, err := filesystemControllerLock(t.Context(), path, true, false); !errors.Is(err, unix.EWOULDBLOCK) {
		if ex != nil {
			ex.Close()
		}
		t.Fatalf("retirement crossed enrollment: %v", err)
	}
	first.Close()
	if ex, err := filesystemControllerLock(t.Context(), path, true, false); !errors.Is(err, unix.EWOULDBLOCK) {
		if ex != nil {
			ex.Close()
		}
		t.Fatalf("retirement crossed remaining enrollment: %v", err)
	}
	second.Close()
	retirement, err := filesystemControllerLock(t.Context(), path, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer retirement.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if lease, err := filesystemControllerLock(ctx, path, false, true); !errors.Is(err, context.DeadlineExceeded) {
		if lease != nil {
			lease.Close()
		}
		t.Fatalf("enrollment crossed retirement: %v", err)
	}
}
func TestFilesystemControllerStateCannotBeExposedOnReuse(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "private", "controllers")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := privateFilesystemControllerState(state, []string{t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{root, alias, filepath.Join(root, "private"), state} {
		var exposed *ExposedTempDirError
		if err := privateFilesystemControllerState(state, []string{source}); !errors.As(err, &exposed) {
			t.Fatalf("controller state exposed through %s: %v", source, err)
		}
	}
}
func TestFilesystemControllerBindingKeepsAdmissionNameSeparate(t *testing.T) {
	parent := "coop-fs-" + strings.Repeat("a", 32)
	controller := "coop-fs-controller-" + strings.Repeat("b", 32)
	boot := "01234567-89ab-cdef-0123-456789abcdef"
	r := filesystemRecord{Parent: parent}
	if r.cgroupParent() != "/"+parent || r.matchesController(fsfilter.Ready{}) {
		t.Fatal("an admission with no controller changed, or matched a ready receipt")
	}
	r.Controller = &filesystemControllerRecord{Name: controller, Ready: &fsfilter.ControllerReady{Birth: fsfilter.ControllerBirth{BootID: boot}}}
	if r.cgroupParent() != "/"+controller+"/"+parent || r.Parent != parent {
		t.Fatal("nested path changed container identity")
	}
	if !r.matchesController(fsfilter.Ready{Controller: controller, BootID: boot}) {
		t.Fatal("matching incarnation rejected")
	}
	for _, ready := range []fsfilter.Ready{{}, {Controller: controller, BootID: "other"}, {Controller: "other", BootID: boot}} {
		if r.matchesController(ready) {
			t.Fatal("foreign incarnation accepted")
		}
	}
}

// A controller whose recorded Docker is gone keeps its custody record. A workspace's pass (a session's
// service start or stop) reports it only when an admission bound it as the pass began: one whose last
// admission this pass retires is that workspace's, one nothing used is not. The host-wide pass, and a
// pass that cannot read every admission, report it either way.
func TestWorkspaceRecoveryReportsOnlyControllersInUse(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir()) // recovery names folders as configured, unresolved
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ServiceStateRootEnv, root)
	d := newFilesystemRecoveryDaemon(t) // the host-wide pass also sweeps controller containers: never the real Docker
	gone := "unix://" + filepath.Join(t.TempDir(), "gone.sock")
	controller := readyControllerRecord(t, "coop-fs-controller-"+strings.Repeat("c", 32), gone)
	dir := writeControllerRecord(t, controller)
	retained := "filesystem controller custody retained at " + dir
	reported := func(err error) bool { return err != nil && strings.Contains(err.Error(), retained) }
	workspace := t.TempDir()
	// Another stale controller whose lease cannot even be opened is reported the same way, by folder.
	locked := writeControllerRecord(t, readyControllerRecord(t, "coop-fs-controller-"+strings.Repeat("b", 32), gone))
	if err := os.Mkdir(filepath.Join(locked, "lease"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := RecoverFilesystemAdmissions(t.Context(), d.rt, workspace); err != nil {
		t.Fatalf("a workspace's pass reported a controller no admission bound: %v", err)
	}
	if _, err := RecoverFilesystemAdmissions(t.Context(), d.rt, ""); !reported(err) || !strings.Contains(err.Error(), "filesystem controller custody retained at "+locked+": ") {
		t.Fatalf("the host-wide pass = %v, want %q and the same for %s", err, retained, locked)
	}
	admission := writeBoundAdmission(t, workspace, controller)
	retire := func(context.Context, runtime.Runtime, string) (int, error) { return 1, os.RemoveAll(admission) }
	if _, err := recoverFilesystem(t.Context(), d.rt, workspace, retire); !reported(err) {
		t.Fatalf("a pass that retired the controller's last admission = %v, want %q", err, retained)
	}
	writeUnreadableAdmission(t)
	if _, err := RecoverFilesystemAdmissions(t.Context(), d.rt, workspace); !reported(err) {
		t.Fatalf("a pass that could not read every admission = %v, want %q", err, retained)
	}
	if _, err := readFilesystemController(dir); err != nil {
		t.Fatalf("the controller's custody record went: %v", err)
	}
	if controllerOfDirectory(controller.Name) != "" || controllerOfDirectory("coop-fs-controller-x-1") != "" {
		t.Fatal("a folder without a controller name and suffix was taken for a controller's")
	}
}

// A controller may go only when no admission binds it. A record that cannot be read is an error, and
// a readable one that binds the controller still counts: either answer keeps it from recovery.
func TestReferencedControllersNamesWhatAdmissionsBind(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	if used, err := referencedControllers(); len(used) != 0 || err != nil {
		t.Fatalf("with no admissions = %v, %v", used, err)
	}
	name := "coop-fs-controller-" + strings.Repeat("c", 32)
	writeBoundAdmission(t, t.TempDir(), readyControllerRecord(t, name, "unix:///runtime.sock"))
	writeUnreadableAdmission(t)
	if used, err := referencedControllers(); len(used) != 1 || !used[name] || err == nil {
		t.Fatalf("referencedControllers = %v, %v; want %s and the unreadable record's error", used, err, name)
	}
	if referenced, err := controllerReferenced(name); !referenced || err != nil {
		t.Fatalf("a controller a readable admission binds = %t, %v; want it referenced", referenced, err)
	}
	if referenced, err := controllerReferenced("coop-fs-controller-" + strings.Repeat("f", 32)); referenced || err == nil {
		t.Fatalf("a controller no readable admission binds = %t, %v; want the read error", referenced, err)
	}
}

// controllerHooks are fsfilter's LSM hooks, one guard program each. When fsfilter's list changes,
// readyControllerRecord fails: copy the new list from internal/fsfilter/protocol.go. It is a copy
// because a helper exported from internal/fsfilter would change the filesystem image's fingerprint
// (internal/fsimage hashes that folder), and so replace every live controller on upgrade.
var controllerHooks = []string{
	"file_open", "file_permission", "file_truncate", "file_receive", "mmap_file", "file_mprotect",
	"inode_setattr", "inode_unlink", "inode_rmdir", "inode_link", "inode_rename", "inode_create",
	"inode_mkdir", "inode_symlink", "inode_mknod", "inode_follow_link", "inode_readlink", "inode_getattr",
	"inode_getxattr", "inode_listxattr", "inode_setxattr", "inode_removexattr", "inode_get_acl",
	"inode_set_acl", "inode_remove_acl", "inode_file_setattr", "inode_file_getattr", "unix_stream_connect",
	"unix_may_send",
}

// readyControllerRecord is a ready controller's record, as its admissions embed it, with made-up
// kernel object IDs.
func readyControllerRecord(t *testing.T, name, endpoint string) filesystemControllerRecord {
	t.Helper()
	birth := fsfilter.ControllerBirth{Version: 1, Root: name, BootID: "01234567-89ab-cdef-0123-456789abcdef", CgroupInode: 10, CgroupDevice: 2, PinDirectoryInode: 20, PinDirectoryDevice: 3}
	for i, m := range []string{"scope", "dispatch", "owners", "runes", "work", "objects", "paths", "devices", "config", "objects_template", "paths_template", "devices_template", "config_template"} {
		birth.Maps = append(birth.Maps, fsfilter.KernelObject{Name: m, ID: uint32(i + 1)})
	}
	birth.Programs = []fsfilter.KernelObject{{Name: "admit_owner", ID: 100}, {Name: "hierarchy_root", ID: 101}, {Name: "release_owner", ID: 102}}
	ready := fsfilter.ControllerReady{Status: "controller-ready"}
	for i, hook := range controllerHooks {
		program := fsfilter.KernelObject{Name: "guard_" + hook, ID: uint32(i + 103)}
		birth.Programs = append(birth.Programs, program)
		ready.Links = append(ready.Links, fsfilter.Link{Name: program.Name, ID: uint32(i + 200), Program: program.ID})
	}
	ready.Birth = birth
	container := filesystemRef(name, "controller")
	container.ID = strings.Repeat("e", 64)
	r := filesystemControllerRecord{Version: 1, Name: name, Directory: name + "-1305127219", Endpoint: endpoint, DaemonID: "daemon",
		Image: "sha256:" + strings.Repeat("d", 64), Container: container, StartedAt: time.Unix(1_800_000_000, 0).UTC(), Ready: &ready}
	if err := r.validate(); err != nil {
		t.Fatalf("the controller fixture no longer matches fsfilter's receipt (see controllerHooks): %v", err)
	}
	return r
}

// writeControllerRecord leaves r in the controllers' state folder and returns its folder.
func writeControllerRecord(t *testing.T, r filesystemControllerRecord) string {
	t.Helper()
	state, err := filesystemControllerState()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, r.Directory)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := filesystemJSON(filepath.Join(dir, "record.json"), r); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeBoundAdmission leaves an admission of workspace that binds controller, and returns its folder.
func writeBoundAdmission(t *testing.T, workspace string, controller filesystemControllerRecord) string {
	t.Helper()
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		t.Fatal(err)
	}
	parent := "coop-fs-" + strings.Repeat("a", 32)
	return writeFilesystemCustody(t, state, filesystemRecord{Version: 1, Parent: parent, Endpoint: controller.Endpoint, DaemonID: controller.DaemonID,
		Workspace: workspace, Supervisor: supervisorLabelValue(workspaceScope(workspace), os.Getpid()), Helper: filesystemRef(parent, "owner"),
		Image: controller.Image, Controller: &controller})
}

// writeUnreadableAdmission leaves an admission folder whose record cannot be read, sorted first.
func writeUnreadableAdmission(t *testing.T) {
	t.Helper()
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "coop-fs-"+strings.Repeat("0", 32)+"-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A controller goes only when its own state folder is gone and nothing else binds under that state
// root; a live box's binds there keep it, and an existing folder always keeps it.
func TestOrphanControllers(t *testing.T) {
	dead := "/tmp/coop-test-state-1/service-state"
	live := "/home/me/.local/state/coop"
	gone := func(path string) bool { return !strings.HasPrefix(path, dead) }
	orphan := ContainerBindsView{ID: "orphan", Binds: []string{"/sys/fs/bpf", dead + "/filesystem-controllers/coop-fs-controller-a-1"}}
	used := ContainerBindsView{ID: "used", Binds: []string{"/tmp/coop-test-state-2/service-state/filesystem-controllers/coop-fs-controller-b-1"}}
	box := ContainerBindsView{ID: "box", Binds: []string{"/tmp/coop-test-state-2/service-state/filesystem/coop-fs-x/policy"}}
	kept := ContainerBindsView{ID: "kept", Binds: []string{live + "/filesystem-controllers/coop-fs-controller-c-1"}}
	other := ContainerBindsView{ID: "other", Binds: []string{"/sys/fs/bpf"}}
	controllers := []ContainerBindsView{orphan, used, kept, other}
	all := append(slices.Clone(controllers), box)
	exists := func(path string) bool { return strings.HasPrefix(path, live) || !gone(path) && false }
	got := orphanControllers(controllers, all, exists)
	if !slices.Equal(got, []string{"orphan"}) {
		t.Errorf("orphans = %q, want only the one nothing uses", got)
	}
}
