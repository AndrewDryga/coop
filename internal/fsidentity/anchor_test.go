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

// A retirement that unlinked the marker and stopped before the private anchor finishes on retry.
func TestRetireFinishesAfterTheMarkerIsAlreadyGone(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	root, err := Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	if err := os.Remove(filepath.Join(binding.RootPath, binding.MarkerName)); err != nil {
		t.Fatal(err)
	}
	if err := Retire(binding); err != nil {
		t.Fatal(err)
	}
	if _, err := binding.AnchorRoot.Lstat(binding.AnchorName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retirement retry left private anchor: %v", err)
	}
}
