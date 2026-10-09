package forkspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func independentAdmissionFixture(t *testing.T) (string, string) {
	t.Helper()
	workspace := t.TempDir()
	lock := filepath.Join(workspace, ".git", "objects", "maintenance.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return workspace, lock
}

// Remove an actual enumerated entry before Info, then mutate only after that
// partial pass has failed. The next attempt must rediscover the complete tree.
func admissionWalkWithVanishingLock(t *testing.T, lock string, after func()) func(string, fs.WalkDirFunc) error {
	t.Helper()
	return func(root string, visit fs.WalkDirFunc) error {
		return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if path != lock || err != nil {
				return visit(path, entry, err)
			}
			if err := os.Remove(lock); err != nil {
				t.Fatal(err)
			}
			err = visit(path, entry, nil)
			if after != nil {
				after()
			}
			return err
		})
	}
}

func TestIndependentGitRestartsAfterEnumeratedEntryDisappears(t *testing.T) {
	workspace, lock := independentAdmissionFixture(t)
	walk := admissionWalkWithVanishingLock(t, lock, nil)
	passes := 0
	err := validateIndependentGit(workspace, func(root string, visit fs.WalkDirFunc) error {
		passes++
		return walk(root, visit)
	})
	if err != nil || passes != 2 {
		t.Fatalf("complete revalidation after disappearance: passes=%d, %v", passes, err)
	}
}

func TestIndependentGitRestartRefusesNewUnsafeMetadata(t *testing.T) {
	for _, attack := range []string{"symlink", "hardlink", "alternate", "missing objects", "gitfile"} {
		t.Run(attack, func(t *testing.T) {
			workspace, lock := independentAdmissionFixture(t)
			metadata := filepath.Join(workspace, ".git")
			foreign := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(foreign, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			mutate := func() {
				var err error
				switch attack {
				case "symlink":
					err = os.Symlink(foreign, filepath.Join(metadata, "foreign"))
				case "hardlink":
					err = os.Link(foreign, filepath.Join(metadata, "foreign"))
				case "alternate":
					err = os.MkdirAll(filepath.Join(metadata, "objects", "info"), 0o700)
					if err == nil {
						err = os.WriteFile(filepath.Join(metadata, "objects", "info", "alternates"), []byte(foreign), 0o600)
					}
				case "missing objects":
					err = os.Remove(filepath.Join(metadata, "objects"))
				case "gitfile":
					err = os.Rename(metadata, metadata+"-saved")
					if err == nil {
						err = os.WriteFile(metadata, []byte("gitdir: "+metadata+"-saved\n"), 0o600)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			err := validateIndependentGit(workspace, admissionWalkWithVanishingLock(t, lock, mutate))
			if err == nil || errors.Is(err, errIndependentGitChanged) {
				t.Fatalf("fresh validation did not refuse %s: %v", attack, err)
			}
		})
	}
}

func TestIndependentGitRestartBoundsPersistentChurn(t *testing.T) {
	workspace, lock := independentAdmissionFixture(t)
	walk := admissionWalkWithVanishingLock(t, lock, nil)
	passes := 0
	err := validateIndependentGit(workspace, func(root string, visit fs.WalkDirFunc) error {
		passes++
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return walk(root, visit)
	})
	if !errors.Is(err, errIndependentGitChanged) || passes != 3 {
		t.Fatalf("persistent metadata mutation admitted or unbounded: passes=%d, %v", passes, err)
	}
}

type permissionDeniedAdmissionEntry struct{ fs.DirEntry }

func (permissionDeniedAdmissionEntry) Info() (fs.FileInfo, error) {
	return nil, os.ErrPermission
}

func TestIndependentGitRestartDoesNotRetryOtherFailures(t *testing.T) {
	for _, failure := range []string{"entry permission", "walk permission", "walk missing", "required objects disappears"} {
		t.Run(failure, func(t *testing.T) {
			workspace, lock := independentAdmissionFixture(t)
			passes := 0
			err := validateIndependentGit(workspace, func(root string, visit fs.WalkDirFunc) error {
				passes++
				if failure == "walk permission" {
					return visit(root, nil, os.ErrPermission)
				}
				if failure == "walk missing" {
					return visit(root, nil, os.ErrNotExist)
				}
				return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
					if failure == "required objects disappears" && path == filepath.Dir(lock) && err == nil {
						if err := os.Remove(lock); err != nil {
							t.Fatal(err)
						}
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					}
					if path == lock && err == nil {
						entry = permissionDeniedAdmissionEntry{entry}
					}
					return visit(path, entry, err)
				})
			})
			if err == nil || passes != 1 || errors.Is(err, errIndependentGitChanged) {
				t.Fatalf("non-transient failure retried or admitted: passes=%d, %v", passes, err)
			}
		})
	}
}
