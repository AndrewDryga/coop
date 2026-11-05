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
