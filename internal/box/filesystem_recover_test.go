package box

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
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
		func(r *filesystemRecord) {
			r.Supervisor = supervisorLabelValue(workspaceScope(t.TempDir()), os.Getpid())
		},
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

// filesystemRecoveryDaemon is a Docker daemon holding containers under their names, answering what
// retiring an admission asks of it, with a docker client that lists no other container.
type filesystemRecoveryDaemon struct {
	mu         sync.Mutex
	containers map[string]runtime.DockerRef
	endpoint   string
	rt         runtime.Runtime
}

func newFilesystemRecoveryDaemon(t *testing.T) *filesystemRecoveryDaemon {
	t.Helper()
	t.Setenv("DOCKER_CONTEXT", "")
	d := &filesystemRecoveryDaemon{containers: map[string]runtime.DockerRef{}}
	d.endpoint = dockersock.ServeHandler(t, http.HandlerFunc(d.serve))
	t.Setenv("DOCKER_HOST", d.endpoint)
	shim := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	d.rt = runtime.Runtime{Name: shim}
	return d
}

func (d *filesystemRecoveryDaemon) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.URL.Path == "/info" {
		_ = json.NewEncoder(w).Encode(dockersock.Info{ID: "fixture-daemon", OSType: "linux", Architecture: "arm64", ServerVersion: "29.4.0", KernelVersion: "fixture"})
		return
	}
	target := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
	for name, ref := range d.containers {
		if target != name && target != ref.ID {
			continue
		}
		if r.Method == http.MethodDelete {
			delete(d.containers, name)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": ref.ID, "Name": "/" + name, "State": map[string]any{"Status": "created"},
			"Config": map[string]any{"Labels": ref.Labels}, "HostConfig": map[string]any{}})
		return
	}
	http.NotFound(w, r)
}

// add puts a container on the daemon under ref's name and labels.
func (d *filesystemRecoveryDaemon) add(ref runtime.DockerRef) {
	d.mu.Lock()
	defer d.mu.Unlock()
	sum := sha256.Sum256([]byte(ref.Name))
	ref.ID = hex.EncodeToString(sum[:])
	d.containers[ref.Name] = ref
}

func (d *filesystemRecoveryDaemon) has(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, present := d.containers[name]
	return present
}

// writeFilesystemCustody leaves record in the state root, as an admission does.
func writeFilesystemCustody(t *testing.T, state string, record filesystemRecord) string {
	t.Helper()
	dir := filepath.Join(state, record.Parent+"-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := lockFilesystemAdmission(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := filesystemJSON(filepath.Join(dir, "record.json"), record); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A timed-out host setup check left a never-started helper on its scratch folder, which it then
// deleted. Recovery looked only per workspace, so nothing ever cleaned it. The pass with no
// workspace takes custody whose workspace is gone once nothing can still use it, and only that: a
// helper protecting a workspace that exists survives even a dead supervisor, because a person still
// works there and that workspace's own recovery decides.
func TestRecoverFilesystemAdmissionsTakesOnlyCustodyOfWorkspacesThatAreGone(t *testing.T) {
	deleted := func(t *testing.T) string { return filepath.Join(t.TempDir(), "deleted-workspace") }
	present := func(t *testing.T) string { return t.TempDir() }
	consumerOf := func(parent string) runtime.DockerRef { return filesystemRef(parent, "consumer") }
	withConsumer := func(r *filesystemRecord) { r.Services, r.Consumers = true, []runtime.DockerRef{consumerOf(r.Parent)} }
	pendingHelper := func(r *filesystemRecord) { r.Pending = r.Helper.Name }
	for _, tc := range []struct {
		name       string
		workspace  func(*testing.T) string
		supervisor func(*testing.T, string) string // goneSupervisorLabel unless set
		edit       func(*filesystemRecord)
		noHelper   bool // the record's helper is not on the daemon
		consumer   bool // the record's consumer is on the daemon
		holdLease  bool
		old        bool // the record was last written long ago
		retired    bool
		wantErr    bool
	}{
		{name: "a dead run's whose workspace is gone", workspace: deleted, retired: true},
		{name: "a timed-out setup check's, whose create Docker finished late", workspace: deleted, edit: pendingHelper, retired: true},
		{name: "a dead run's whose workspace exists", workspace: present},
		{name: "a live supervisor's whose workspace is gone", workspace: deleted, supervisor: liveSupervisorLabel},
		{name: "one whose lease a process holds", workspace: deleted, holdLease: true},
		{name: "another host's", workspace: deleted, supervisor: func(_ *testing.T, w string) string { return supervisorLabelOn(otherHost, w, os.Getpid()) }},
		{name: "another host's services", workspace: deleted, edit: withConsumer, supervisor: func(_ *testing.T, w string) string { return supervisorLabelOn(otherHost, w, os.Getpid()) }},
		{name: "services with a consumer still there", workspace: deleted, edit: withConsumer, consumer: true},
		{name: "services whose consumers are gone", workspace: deleted, edit: withConsumer, retired: true},
		{name: "a create that has not landed yet", workspace: deleted, edit: pendingHelper, noHelper: true, wantErr: true},
		{name: "a create that never landed", workspace: deleted, edit: pendingHelper, noHelper: true, old: true, retired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ServiceStateRootEnv, t.TempDir())
			state, err := serviceStateRoot("filesystem")
			if err != nil {
				t.Fatal(err)
			}
			d := newFilesystemRecoveryDaemon(t)
			workspace := tc.workspace(t)
			supervisor := goneSupervisorLabel(t, workspace)
			if tc.supervisor != nil {
				supervisor = tc.supervisor(t, workspace)
			}
			parent := "coop-fs-" + strings.Repeat("a", 32)
			record := filesystemRecord{Version: 1, Parent: parent, Endpoint: d.endpoint, DaemonID: "fixture-daemon", Workspace: workspace,
				Supervisor: supervisor, Helper: filesystemRef(parent, "owner")}
			if tc.edit != nil {
				tc.edit(&record)
			}
			dir := writeFilesystemCustody(t, state, record)
			if tc.old {
				when := time.Now().Add(-unfinishedCreateSettle - time.Minute)
				if err := os.Chtimes(filepath.Join(dir, "record.json"), when, when); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.noHelper {
				d.add(record.Helper)
			}
			if tc.consumer {
				d.add(consumerOf(parent))
			}
			if tc.holdLease {
				lease, err := lockFilesystemAdmission(dir, false)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
			}
			removed, err := recoverFilesystemAdmissions(t.Context(), d.rt, "")
			if (err != nil) != tc.wantErr {
				t.Fatalf("recovery = %d, %v; want an error: %v", removed, err, tc.wantErr)
			}
			if tc.retired {
				if removed != 1 || exists(dir) || d.has(record.Helper.Name) {
					t.Fatalf("recovery = %d (custody left: %v, helper left: %v), want it retired", removed, exists(dir), d.has(record.Helper.Name))
				}
				return
			}
			if removed != 0 || !exists(dir) || !tc.noHelper && !d.has(record.Helper.Name) || tc.consumer && !d.has(consumerOf(parent).Name) {
				t.Fatalf("recovery = %d (custody left: %v, helper left: %v), want everything kept", removed, exists(dir), d.has(record.Helper.Name))
			}
			if got, err := readFilesystemRecord(dir); err != nil || got.Pending != record.Pending {
				t.Fatalf("recovery changed what the record says is pending: %+v %v", got, err)
			}
		})
	}
}

// A workspace's own recovery still takes only that workspace's custody, gone or not.
func TestRecoverFilesystemAdmissionsForAWorkspaceLeavesAnothersCustody(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		t.Fatal(err)
	}
	d := newFilesystemRecoveryDaemon(t)
	mine, other := t.TempDir(), filepath.Join(t.TempDir(), "deleted-workspace")
	parent := "coop-fs-" + strings.Repeat("b", 32)
	record := filesystemRecord{Version: 1, Parent: parent, Endpoint: d.endpoint, DaemonID: "fixture-daemon", Workspace: other,
		Supervisor: goneSupervisorLabel(t, other), Helper: filesystemRef(parent, "owner")}
	dir := writeFilesystemCustody(t, state, record)
	d.add(record.Helper)
	if removed, err := recoverFilesystemAdmissions(t.Context(), d.rt, mine); err != nil || removed != 0 || !exists(dir) || !d.has(record.Helper.Name) {
		t.Fatalf("recovery for another workspace = %d, %v (custody left: %v)", removed, err, exists(dir))
	}
}

// A workspace spelled through a link keeps the scope of its real path, which that spelling no longer
// resolves to once the workspace is gone. Its record must still read, or it would stay forever and
// fail every controller recovery that reads it.
func TestFilesystemRecordOfAGoneLinkedWorkspaceStaysReadable(t *testing.T) {
	root := t.TempDir()
	real, link := filepath.Join(root, "real"), filepath.Join(root, "link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	parent := "coop-fs-" + strings.Repeat("c", 32)
	record := filesystemRecord{Version: 1, Parent: parent, Endpoint: "unix:///runtime.sock", DaemonID: "daemon", Workspace: link,
		Supervisor: goneSupervisorLabel(t, link), Helper: filesystemRef(parent, "owner")}
	if err := record.validate(); err != nil {
		t.Fatalf("a live linked workspace: %v", err)
	}
	if err := os.RemoveAll(real); err != nil {
		t.Fatal(err)
	}
	if err := record.validate(); err != nil {
		t.Fatalf("a linked workspace whose target is gone: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := record.validate(); err != nil {
		t.Fatalf("a linked workspace that is gone: %v", err)
	}
}

// Only "no such file" means gone: a workspace the check cannot read may still be in use.
func TestFilesystemWorkspaceGoneOnlyWhenTheCheckSaysSo(t *testing.T) {
	if filesystemWorkspaceGone(t.TempDir()) {
		t.Fatal("a workspace that exists counted as gone")
	}
	if !filesystemWorkspaceGone(filepath.Join(t.TempDir(), "deleted")) {
		t.Fatal("a removed workspace counted as there")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder whatever its mode")
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if filesystemWorkspaceGone(filepath.Join(locked, "workspace")) {
		t.Fatal("a workspace the check cannot read counted as gone")
	}
}
