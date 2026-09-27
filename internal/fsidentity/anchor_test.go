package fsidentity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func testBinding(t *testing.T) (Binding, func()) {
	t.Helper()
	parent := t.TempDir()
	rootPath := filepath.Join(parent, "workspace")
	statePath := filepath.Join(parent, "state")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := os.OpenRoot(statePath)
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{RootPath: rootPath, MarkerName: ".coop-anchor", AnchorRoot: state,
		AnchorName: "project.anchor", Body: []byte("coop-anchor-v1\n")}
	return binding, func() { _ = state.Close() }
}

func TestAnchorSurvivesReopenAndRejectsCopiedMarker(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	root, err := Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	root, err = Open(binding)
	if err != nil {
		t.Fatalf("reopen binding: %v", err)
	}
	_ = root.Close()

	marker := filepath.Join(binding.RootPath, binding.MarkerName)
	copyPath := filepath.Join(binding.RootPath, "copy")
	if err := os.WriteFile(copyPath, binding.Body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(copyPath, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(binding); err == nil {
		t.Fatal("copied marker bytes matched the private anchor")
	}
}

func TestAnchorRejectsMissingSymlinkAndExtraLink(t *testing.T) {
	for _, mutate := range []struct {
		name string
		fn   func(t *testing.T, binding Binding)
	}{
		{name: "missing", fn: func(t *testing.T, binding Binding) {
			if err := os.Remove(filepath.Join(binding.RootPath, binding.MarkerName)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", fn: func(t *testing.T, binding Binding) {
			path := filepath.Join(binding.RootPath, binding.MarkerName)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(binding.AnchorRoot.Name(), binding.AnchorName), path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "extra-link", fn: func(t *testing.T, binding Binding) {
			if err := os.Link(filepath.Join(binding.RootPath, binding.MarkerName), filepath.Join(binding.RootPath, "third")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			binding, closeState := testBinding(t)
			defer closeState()
			root, err := Create(binding)
			if err != nil {
				t.Fatal(err)
			}
			_ = root.Close()
			mutate.fn(t, binding)
			if _, err := Open(binding); err == nil {
				t.Fatal("mutated marker remained authoritative")
			}
		})
	}
}

func TestAnchorRejectsReplacementRootEvenWhenMarkerBytesMatch(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	root, err := Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	old := binding.RootPath + ".old"
	if err := os.Rename(binding.RootPath, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(binding.RootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binding.RootPath, binding.MarkerName), binding.Body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(binding); err == nil {
		t.Fatal("replacement root inherited the old anchor")
	}
}

func TestAnchorLinkFailureCleansBothNames(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	previous := linkNames
	linkNames = func(*os.Root, string, *os.Root, string) error { return syscall.EXDEV }
	t.Cleanup(func() { linkNames = previous })
	if _, err := Create(binding); err == nil || !strings.Contains(err.Error(), "one filesystem") {
		t.Fatalf("Create error = %v", err)
	}
	for _, path := range []string{
		filepath.Join(binding.RootPath, binding.MarkerName),
		filepath.Join(binding.AnchorRoot.Name(), binding.AnchorName),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed creation left %s: %v", path, err)
		}
	}
}

func TestAnchorDoesNotPublishMarkerBeforePrivateNameIsDurable(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	previousSync, previousLink := syncPrivateAnchorRoot, linkNames
	syncPrivateAnchorRoot = func(*os.Root) error { return syscall.EIO }
	linked := false
	linkNames = func(*os.Root, string, *os.Root, string) error {
		linked = true
		return nil
	}
	t.Cleanup(func() {
		syncPrivateAnchorRoot = previousSync
		linkNames = previousLink
	})

	if _, err := Create(binding); err == nil || !strings.Contains(err.Error(), "sync private") {
		t.Fatalf("Create error = %v, want private-directory sync failure", err)
	}
	if linked {
		t.Fatal("Create linked the public marker before the private name was durable")
	}
	for _, path := range []string{
		filepath.Join(binding.RootPath, binding.MarkerName),
		filepath.Join(binding.AnchorRoot.Name(), binding.AnchorName),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed creation left %s: %v", path, err)
		}
	}
}

func TestAnchorCreateCleanupRetainsPrivateNameUntilPublicRemovalIsDurable(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	previousPublish, previousCleanup := syncCreatedPublicRoot, syncCreateCleanupPublicRoot
	publishFailure := errors.New("synthetic marker publication sync failure")
	cleanupFailure := errors.New("synthetic marker removal sync failure")
	syncCreatedPublicRoot = func(*os.Root) error { return publishFailure }
	syncCreateCleanupPublicRoot = func(*os.Root) error { return cleanupFailure }
	t.Cleanup(func() {
		syncCreatedPublicRoot = previousPublish
		syncCreateCleanupPublicRoot = previousCleanup
	})

	if _, err := Create(binding); !errors.Is(err, publishFailure) || !errors.Is(err, cleanupFailure) {
		t.Fatalf("Create error = %v, want publication and cleanup barriers", err)
	}
	if _, err := os.Lstat(filepath.Join(binding.RootPath, binding.MarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed cleanup left marker visibly present: %v", err)
	}
	if _, err := binding.AnchorRoot.Lstat(binding.AnchorName); err != nil {
		t.Fatalf("failed public cleanup barrier removed the private recovery anchor: %v", err)
	}

	// Once the same public barrier succeeds, ordinary retirement can remove the retained name.
	syncCreateCleanupPublicRoot = previousCleanup
	if err := Retire(binding); err != nil {
		t.Fatal(err)
	}
	if _, err := binding.AnchorRoot.Lstat(binding.AnchorName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retirement left retained create anchor: %v", err)
	}
}

func TestAnchorRemovalRequiresTheExactBinding(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	root, err := Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	wrong := binding
	wrong.Body = []byte("another binding\n")
	if err := Retire(wrong); err == nil {
		t.Fatal("mismatched cleanup removed authority")
	}
	valid, err := Open(binding)
	if err != nil {
		t.Fatalf("wrong binding damaged the valid authority: %v", err)
	}
	_ = valid.Close()
	if err := Retire(binding); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(binding.RootPath, binding.MarkerName),
		filepath.Join(binding.AnchorRoot.Name(), binding.AnchorName),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed binding left %s: %v", path, err)
		}
	}
}

func TestRetireCleansPrivateAnchorAfterRootRemoval(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	root, err := Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	if err := os.RemoveAll(binding.RootPath); err != nil {
		t.Fatal(err)
	}
	if err := Retire(binding); err != nil {
		t.Fatal(err)
	}
	if _, err := binding.AnchorRoot.Lstat(binding.AnchorName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired root left private anchor: %v", err)
	}
}

func TestRetireRetrySyncsMissingPublicMarkerBeforeRemovingLastAnchor(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	root, err := Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()

	previous := syncRetiredPublicRoot
	t.Cleanup(func() { syncRetiredPublicRoot = previous })
	failure := errors.New("synthetic public directory sync failure")
	syncRetiredPublicRoot = func(*os.Root) error { return failure }
	if err := Retire(binding); !errors.Is(err, failure) {
		t.Fatalf("first retirement error = %v, want %v", err, failure)
	}
	if _, err := os.Lstat(filepath.Join(binding.RootPath, binding.MarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed public sync left marker visible: %v", err)
	}
	if _, err := binding.AnchorRoot.Lstat(binding.AnchorName); err != nil {
		t.Fatalf("failed public sync removed the last private anchor: %v", err)
	}

	publicSynced := false
	syncRetiredPublicRoot = func(root *os.Root) error {
		publicSynced = true
		if _, err := binding.AnchorRoot.Lstat(binding.AnchorName); err != nil {
			t.Fatalf("public retry barrier ran after private anchor removal: %v", err)
		}
		return syncRoot(root)
	}
	if err := Retire(binding); err != nil {
		t.Fatal(err)
	}
	if !publicSynced {
		t.Fatal("retirement retry skipped the public directory durability barrier")
	}
	if _, err := binding.AnchorRoot.Lstat(binding.AnchorName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retirement retry left private anchor: %v", err)
	}
}
