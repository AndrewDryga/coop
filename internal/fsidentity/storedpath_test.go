package fsidentity

import (
	"os"
	"path/filepath"
	"testing"
)

// On a disk that ignores letter case, two spellings of one folder give one stored path, the
// listing's; on a disk that keeps case, they are two folders and keep their own paths. The test asks
// the disk which it is.
func TestStoredPathIsTheDisksSpelling(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "Projects", "App")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	upper, err := os.Lstat(project)
	if err != nil {
		t.Fatal(err)
	}
	lower, lowerErr := os.Lstat(filepath.Join(root, "projects", "app"))
	folds := lowerErr == nil && os.SameFile(upper, lower)
	if !folds {
		if err := os.MkdirAll(filepath.Join(root, "projects", "app"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	for _, spelling := range []string{project, filepath.Join(root, "projects", "app"), filepath.Join(root, "PROJECTS", "aPP")} {
		got, err := StoredPath(spelling)
		if !folds && spelling != project && err == nil && got == project {
			t.Fatalf("StoredPath(%s) = %s on a disk that keeps case: two folders shared one path", spelling, got)
		}
		if folds && (err != nil || got != project) {
			t.Fatalf("StoredPath(%s) = %q, %v; want the stored spelling %s", spelling, got, err, project)
		}
	}
	if got, err := StoredPath(project); err != nil || got != project {
		t.Fatalf("StoredPath of the stored spelling = %q, %v", got, err)
	}

	// Links resolve, and a missing path comes back absolute as given.
	link := filepath.Join(root, "link")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	if got, err := StoredPath(link); err != nil || got != project {
		t.Fatalf("StoredPath(link) = %q, %v; want %s", got, err, project)
	}
	missing := filepath.Join(root, "Missing", "Child")
	if got, err := StoredPath(missing); err != nil || got != missing {
		t.Fatalf("StoredPath(missing) = %q, %v; want it as given", got, err)
	}
}
