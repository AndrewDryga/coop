package box

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
	"github.com/AndrewDryga/coop/internal/ui"
)

func legacyMountRuntime(t *testing.T, source, status string) runtime.Runtime {
	t.Helper()
	return legacyMountRuntimeWith(t, source, status, mountListingHooks{})
}

func legacyMountRuntimeWith(t *testing.T, source, status string, hooks mountListingHooks) runtime.Runtime {
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
	script := "#!/bin/sh\ncase \"$*\" in\n*\"ps -q\"*) echo pass >> \"$0.passes\"\n" + hooks.ps + "\nprintf '%s\\n' '" + id + "' ;;\n" +
		"*\"inspect --type container --format\"*) " + hooks.inspect + "\nprintf '%s\\n' " + strconv.Quote(string(raw)) + " ;;\nesac\n"
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

// mountListingHooks run inside the fixture's docker before it answers `ps` or `inspect`. A hook that
// hangs execs its sleep, so the client's kill leaves nothing holding the fixture's output open.
type mountListingHooks struct{ ps, inspect string }

// quickDockerListing shortens the patient listing's limits for a test that expects to exhaust them.
func quickDockerListing(t *testing.T) {
	t.Helper()
	attempt, limit, pause := dockerListingAttemptLimit, dockerListingLimit, dockerListingPause
	t.Cleanup(func() { dockerListingAttemptLimit, dockerListingLimit, dockerListingPause = attempt, limit, pause })
	dockerListingAttemptLimit, dockerListingLimit, dockerListingPause = 300*time.Millisecond, 2*time.Second, 10*time.Millisecond
}

// listingPasses is how many times the fixture's docker was asked to list containers.
func listingPasses(t *testing.T, rt runtime.Runtime) int {
	t.Helper()
	data, err := os.ReadFile(rt.Name + ".passes")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// The launch of 2026-11-04 stopped on a host with 47 containers and other sessions running tests.
// The listing takes 0.15 seconds there when the host is quiet, so this is load, not a real problem.
func TestLegacyMountListingSurvivesASlowDocker(t *testing.T) {
	home, unrelated := t.TempDir(), t.TempDir()
	rt := legacyMountRuntimeWith(t, unrelated, "running", mountListingHooks{ps: "sleep 5.5"})
	start := time.Now()
	if err := checkLegacyMountPaths(t.Context(), rt, false, home); err != nil {
		t.Fatalf("slow listing refused: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Second {
		t.Fatalf("listing took %v, so the test did not pass the old limit", elapsed)
	}
	if passes := listingPasses(t, rt); passes != 1 {
		t.Fatalf("a slow listing that finished took %d passes", passes)
	}
}

// A container removed between `ps` and `inspect` fails the pass. The next pass decides from scratch,
// so a sign-in folder it finds mounted is still refused.
func TestLegacyMountListingAsksAgainWhenContainersChurn(t *testing.T) {
	quickDockerListing(t)
	failOnce := `[ -e "$0.churned" ] || { : > "$0.churned"; echo 'No such container' >&2; exit 1; }`
	home, unrelated := t.TempDir(), t.TempDir()
	rt := legacyMountRuntimeWith(t, unrelated, "running", mountListingHooks{inspect: failOnce})
	if err := checkLegacyMountPaths(t.Context(), rt, false, home); err != nil {
		t.Fatalf("churn refused the launch: %v", err)
	}
	if passes := listingPasses(t, rt); passes != 2 {
		t.Fatalf("passes = %d, want 2", passes)
	}
	rt = legacyMountRuntimeWith(t, home, "exited", mountListingHooks{inspect: failOnce})
	err := checkLegacyMountPaths(t.Context(), rt, false, home)
	if err == nil || !strings.Contains(err.Error(), "is mounted by a running or restartable container") {
		t.Fatalf("second pass did not judge the mount: %v", err)
	}
}

// The source would pass the check, so a listing taken for empty or partial would let the launch go.
func TestLegacyMountListingRefusesWhenDockerCannotAnswer(t *testing.T) {
	const secret = "SECRET-CANARY-daemon-error"
	for _, test := range []struct {
		name, ps, headline, advice string
	}{
		{"never answers", "exec sleep 300", "Docker did not answer in time", "Try again."},
		{"fails", "echo " + secret + " >&2; exit 1", "Docker could not list its containers", "Try again when Docker is ready."},
	} {
		t.Run(test.name, func(t *testing.T) {
			quickDockerListing(t)
			rt := legacyMountRuntimeWith(t, t.TempDir(), "running", mountListingHooks{ps: test.ps})
			err := checkLegacyMountPaths(t.Context(), rt, false, t.TempDir())
			var refusal *ui.UsageError
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			want := "Coop could not check which containers hold this saved sign-in.\nNothing was changed. " + test.advice
			if refusal.Headline != test.headline || refusal.Cause != want || refusal.ExitCode != 1 {
				t.Fatalf("refusal = %+v", refusal)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("refusal carries Docker's error: %v", err)
			}
			if passes := listingPasses(t, rt); passes != dockerListingAttempts {
				t.Fatalf("passes = %d, want %d", passes, dockerListingAttempts)
			}
		})
	}
}

func TestDockerListingKeepsItsOverallLimit(t *testing.T) {
	quickDockerListing(t)
	dockerListingAttemptLimit, dockerListingLimit = time.Minute, 200*time.Millisecond
	rt := legacyMountRuntimeWith(t, t.TempDir(), "running", mountListingHooks{ps: "exec sleep 300"})
	err := checkLegacyMountPaths(t.Context(), rt, false, t.TempDir())
	var refusal *ui.UsageError
	if !errors.As(err, &refusal) || refusal.Headline != "Docker did not answer in time" {
		t.Fatalf("err = %v", err)
	}
	if passes := listingPasses(t, rt); passes != 1 {
		t.Fatalf("passes = %d after the overall limit, want 1", passes)
	}
}

func TestDockerListingStopsWhenTheCallerCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	err := listDockerPatiently(ctx, func(ctx context.Context) error {
		calls++
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err = %v after %d calls", err, calls)
	}
}

// The account's own runtime check runs first on the same launch, so it needs the same patience.
func TestCredentialContainerCheckToleratesASlowDocker(t *testing.T) {
	const failOnce = `[ -e "$0.churned" ] || { : > "$0.churned"; exit 1; }`
	for _, test := range []struct {
		name, ps, headline string
		busy               bool
	}{
		{name: "asks again after a failed pass", ps: failOnce},
		{name: "a container that holds it is still found", ps: failOnce + "; printf '%s\\n' abc", busy: true},
		{name: "never answers", ps: "exec sleep 300", headline: "Docker did not answer in time"},
		{name: "fails", ps: "exit 1", headline: "Docker could not list its containers"},
	} {
		t.Run(test.name, func(t *testing.T) {
			quickDockerListing(t)
			path := filepath.Join(t.TempDir(), "docker")
			script := "#!/bin/sh\ncase \"$*\" in\n*\"ps -q\"*) " + test.ps + " ;;\n*inspect*) printf '%s\\n' '{\"k\":\"v\"}' ;;\nesac\n"
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			err := checkCredentialContainers(t.Context(), runtime.Runtime{Name: path}, "claude", "key", true)
			var refusal *ui.UsageError
			switch {
			case test.headline != "":
				if !errors.As(err, &refusal) || refusal.Headline != test.headline {
					t.Fatalf("err = %v, want %q", err, test.headline)
				}
			case test.busy:
				if err == nil || !strings.Contains(err.Error(), "credential is busy") {
					t.Fatalf("err = %v, want busy", err)
				}
			case err != nil:
				t.Fatalf("err = %v", err)
			}
		})
	}
}
