package box

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func legacyMountRuntime(t *testing.T, source, status string) runtime.Runtime {
	t.Helper()
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "legacy-fixture", OSType: "linux", Architecture: "amd64", ServerVersion: "29", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
	}))
	id := strings.Repeat("a", 64)
	record := map[string]any{"ID": id, "Status": status, "Mounts": []map[string]any{{"Type": "bind", "Source": source, "RW": false}}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\ncase \"$*\" in\n*\"ps -q\"*) printf '%s\\n' '" + id + "' ;;\n" +
		"*\"inspect --type container --format\"*) printf '%s\\n' " + strconv.Quote(string(raw)) + " ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: path}
}

func TestLegacyMountPathsResolveOnlySafeStoppedMissingTails(t *testing.T) {
	home := t.TempDir()
	unrelated := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(t.TempDir(), "dangling")
	if err := os.Symlink(filepath.Join(home, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	nonDirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(nonDirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, source, status string
		allowed              bool
	}{
		{"unrelated existing live bind", unrelated, "running", true},
		{"unrelated removed leaf", filepath.Join(unrelated, "removed"), "exited", true},
		{"unrelated removed subtree", filepath.Join(unrelated, "removed", "nested"), "created", true},
		{"unresolved running bind", filepath.Join(unrelated, "removed"), "running", false},
		{"unresolved paused bind", filepath.Join(unrelated, "removed"), "paused", false},
		{"unresolved restarting bind", filepath.Join(unrelated, "removed"), "restarting", false},
		{"unresolved dead bind", filepath.Join(unrelated, "removed"), "dead", false},
		{"exact home stopped readonly", home, "exited", false},
		{"home ancestor", filepath.Dir(home), "created", false},
		{"removed home descendant", filepath.Join(home, "removed"), "exited", false},
		{"home alias", alias, "exited", false},
		{"removed aliased descendant", filepath.Join(alias, "removed"), "exited", false},
		{"dangling alias", dangling, "exited", false},
		{"nested dangling alias", filepath.Join(dangling, "nested"), "exited", false},
		{"symlink loop", loop, "exited", false},
		{"relative source", "relative/missing", "exited", false},
		{"non directory ancestor", filepath.Join(nonDirectory, "child"), "exited", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := legacyMountRuntime(t, test.source, test.status)
			err := checkLegacyMountPaths(t.Context(), rt, false, home)
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v, want%v: %v", err == nil, test.allowed, err)
			}
		})
	}
}

// OrbStack shows the Mac's /Users/me at /mnt/mac/Users/me and Docker Desktop at /host_mnt/Users/me.
// Neither VM path exists on the host, so a stopped container binding the sign-in folder that way was
// judged as an unrelated missing path and passed, though `docker start` would mount the folder
// again. Such a source is judged as the host path it names; an unrelated missing source still passes,
// and a running one still fails closed.
func TestLegacyMountPathsMapTheVMViewOfHostFolders(t *testing.T) {
	home, unrelated := t.TempDir(), t.TempDir()
	for _, test := range []struct {
		name, source, status string
		allowed              bool
	}{
		{"Docker Desktop view of the sign-in folder", "/host_mnt" + home, "exited", false},
		{"OrbStack view of the sign-in folder", "/mnt/mac" + home, "created", false},
		{"Docker Desktop view of a removed descendant", "/host_mnt" + filepath.Join(home, "removed"), "exited", false},
		{"OrbStack view of an ancestor", "/mnt/mac" + filepath.Dir(home), "exited", false},
		{"OrbStack view of the whole host", "/mnt/mac", "exited", false},
		{"Docker Desktop view of the whole host", "/host_mnt", "created", false},
		{"unclean OrbStack view", "/mnt/mac/." + home, "exited", false},
		{"Docker Desktop view of an unrelated removed folder", "/host_mnt" + filepath.Join(unrelated, "removed"), "exited", true},
		{"OrbStack view of an unrelated folder", "/mnt/mac" + unrelated, "created", true},
		{"unrelated missing VM path", "/mnt/coop-test-missing" + home, "exited", true},
		{"name that only starts like the OrbStack view", "/mnt/macintosh" + home, "exited", true},
		{"running OrbStack view of an unrelated folder", "/mnt/mac" + unrelated, "running", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := legacyMountRuntime(t, test.source, test.status)
			err := checkLegacyMountPaths(t.Context(), rt, false, home)
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v, want %v: %v", err == nil, test.allowed, err)
			}
		})
	}
}

func TestLegacyMountPathsRefuseInaccessibleAncestor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can traverse mode000 directories")
	}
	denied := filepath.Join(t.TempDir(), "denied")
	if err := os.Mkdir(denied, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o700) })
	rt := legacyMountRuntime(t, filepath.Join(denied, "missing"), "exited")
	if err := checkLegacyMountPaths(t.Context(), rt, false, t.TempDir()); err == nil {
		t.Fatal("unavailable ancestor treated as harmless absence")
	}
}

func TestAuthorityPathMissingTailDoesNotHideDanglingAlias(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "removed"), alias); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{alias, filepath.Join(alias, "nested")} {
		if _, err := resolveAuthorityPath(source); err == nil {
			t.Fatal("dangling alias reconstructed as an unrelated missing tail")
		}
	}
}

// Docker Desktop and OrbStack run the daemon in a Linux VM. Coop's own filesystem helpers bind the
// VM's /sys/fs/cgroup and /sys/fs/bpf, which a Mac does not have; those binds must not block every
// provider launch, while anything else the host cannot resolve keeps failing closed. Every row
// behaves the same on macOS and on Linux, where /sys exists and the roots resolve normally.
func TestLegacyMountPathsSkipOnlyHostlessKernelMounts(t *testing.T) {
	home := t.TempDir()
	previous := hostLstat
	t.Cleanup(func() { hostLstat = previous })
	hostLstat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	for _, test := range []struct {
		name, source string
		allowed      bool
	}{
		{"VM bpffs root", "/sys/fs/bpf", true},
		{"VM cgroup root", "/sys/fs/cgroup", true},
		{"VM sysfs root", "/sys", true},
		{"path below a kernel root", "/sys/fs/bpf/coop-test-missing", false},
		{"other missing sysfs path", "/sys/coop-test-missing", false},
		{"unclean path that leaves /sys", "/sys/.." + filepath.Join(home, "missing"), false},
		{"VM view of a host folder", "/mnt/coop-test-host" + home, false},
		{"name that only starts like sys", "/sysfs-coop-test/missing", false},
		{"relative sys path", "sys/fs/bpf", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := legacyMountRuntime(t, test.source, "running")
			err := checkLegacyMountPaths(t.Context(), rt, false, home)
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v, want %v: %v", err == nil, test.allowed, err)
			}
			if err != nil && !strings.Contains(err.Error(), strconv.Quote(test.source)) {
				t.Fatalf("refusal does not name the mount %q: %v", test.source, err)
			}
		})
	}
}

func TestHostlessKernelMountNeedsAHostWithoutSys(t *testing.T) {
	previous := hostLstat
	t.Cleanup(func() { hostLstat = previous })
	hostSys, err := os.Lstat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		lstat      func(string) (os.FileInfo, error)
		source     string
		hostlessOK bool
	}{
		{"host without /sys", func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, "/sys/fs/bpf", true},
		{"host with /sys", func(string) (os.FileInfo, error) { return hostSys, nil }, "/sys/fs/bpf", false},
		{"unreadable /sys", func(string) (os.FileInfo, error) { return nil, os.ErrPermission }, "/sys/fs/cgroup", false},
		{"below a root", func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, "/sys/fs/bpf/link", false},
		{"trailing slash", func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, "/sys/fs/bpf/", false},
		// /proc/<pid>/root inside a bound procfs reaches every filesystem the VM sees, so even its
		// root never counts as a kernel-only bind.
		{"procfs root", func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, "/proc", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			hostLstat = test.lstat
			if got := hostlessKernelMount(test.source); got != test.hostlessOK {
				t.Fatalf("hostlessKernelMount(%q) = %v, want %v", test.source, got, test.hostlessOK)
			}
		})
	}
}
