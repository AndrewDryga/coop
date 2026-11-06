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

// overwriteInPlace writes the marker the way `printf junk > marker` does: through its inode, which
// the private anchor shares.
func overwriteInPlace(t *testing.T, binding Binding, data []byte) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(binding.RootPath, binding.MarkerName), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// A marker written in place changes the private anchor too, so its bytes prove nothing and Retire,
// which checks them, can never finish. Discard retires the pair by identity instead: the marker
// while it is the anchor's own inode, then the private name.
func TestDiscardRetiresAnOverwrittenPairByIdentity(t *testing.T) {
	for name, alter := range map[string]func(t *testing.T, binding Binding){
		"overwritten": func(t *testing.T, binding Binding) { overwriteInPlace(t, binding, []byte("junk\n")) },
		"grown":       func(t *testing.T, binding Binding) { overwriteInPlace(t, binding, make([]byte, maxAnchorBody+1)) },
		"same size":   func(t *testing.T, binding Binding) { overwriteInPlace(t, binding, []byte("coop-anchor-v2\n")) },
		"chmod": func(t *testing.T, binding Binding) {
			if err := os.Chmod(filepath.Join(binding.RootPath, binding.MarkerName), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			binding, closeState := testBinding(t)
			defer closeState()
			root, err := Create(binding)
			if err != nil {
				t.Fatal(err)
			}
			_ = root.Close()
			if altered, err := Altered(binding); err != nil || altered {
				t.Fatalf("an intact binding: altered=%v err=%v", altered, err)
			}
			alter(t, binding)
			if _, err := Open(binding); err == nil {
				t.Fatal("an altered pair still proves the binding")
			}
			if altered, err := Altered(binding); err != nil || !altered {
				t.Fatalf("altered=%v err=%v, want the change seen", altered, err)
			}
			if err := Retire(binding); err == nil {
				t.Fatal("Retire removed a pair its bytes no longer prove")
			}
			if err := Discard(binding); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{
				filepath.Join(binding.RootPath, binding.MarkerName),
				filepath.Join(binding.AnchorRoot.Name(), binding.AnchorName),
			} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("discarded binding left %s: %v", path, err)
				}
			}
			if err := Discard(binding); err != nil {
				t.Fatal("a second discard of nothing failed:", err)
			}
		})
	}
}

// Identity, not the name, decides what Discard removes: a marker that is another file stays, and a
// binding whose bytes still prove it is refused, for Retire to check.
func TestDiscardRemovesOnlyThePairItself(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	root, err := Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	if err := Discard(binding); err == nil || !strings.Contains(err.Error(), "retire it instead") {
		t.Fatalf("Discard of an intact binding = %v", err)
	}
	if root, err := Open(binding); err != nil {
		t.Fatal("a refused discard damaged the binding:", err)
	} else {
		_ = root.Close()
	}
	overwriteInPlace(t, binding, []byte("junk\n"))
	marker := filepath.Join(binding.RootPath, binding.MarkerName)
	stranger := filepath.Join(binding.RootPath, "stranger")
	if err := os.WriteFile(stranger, []byte("junk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stranger, marker); err != nil {
		t.Fatal(err)
	}
	if err := Discard(binding); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(marker); err != nil {
		t.Fatal("Discard removed a marker that is another file:", err)
	}
	if _, err := binding.AnchorRoot.Lstat(binding.AnchorName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the altered private anchor stayed: %v", err)
	}
}

// A marker matched to the private anchor is removed only while the private name still holds that
// inode. If something replaced the pair in between, as a concurrent review can, the marker may no
// longer be the file Discard matched, so it stays.
func TestDiscardKeepsTheMarkerWhenThePrivateNameChangesFirst(t *testing.T) {
	binding, closeState := testBinding(t)
	defer closeState()
	root, err := Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	overwriteInPlace(t, binding, []byte("junk\n"))
	beforeDiscardRemovesMarker = func() {
		if err := binding.AnchorRoot.Remove(binding.AnchorName); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binding.AnchorRoot.Name(), binding.AnchorName), []byte("newer\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeDiscardRemovesMarker = nil })
	if err := Discard(binding); err == nil {
		t.Fatal("Discard went on after the private name changed under it")
	}
	if _, err := os.Lstat(filepath.Join(binding.RootPath, binding.MarkerName)); err != nil {
		t.Fatal("Discard removed a marker it could no longer prove:", err)
	}
}
