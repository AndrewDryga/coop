package box

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFilesystemObjectsMatchFullWalk(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a/z", "a.txt", "!first", ".git", "secrets/nested", "probe"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "entry"), []byte("inert"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(root, "secrets/nested/entry"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secrets", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	expected := map[string]filesystemObject{}
	var order []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "probe" {
			return fs.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		id, ok := identityOf(info)
		if !ok {
			t.Fatal("no identity")
		}
		expected[rel] = filesystemObject{id: id, mode: info.Mode().Type()}
		order = append(order, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		got, err := filesystemObjects(root, "probe")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.objects, expected) {
			t.Fatal("parallel inventory differs from full no-follow walk")
		}
		if gotOrder := got.paths; !slices.Equal(order, gotOrder) {
			t.Fatalf("walk order differs: %q versus %q", gotOrder, order)
		}
	}
}

func TestFilesystemObjectsRefuseInvalidRoot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, link, filepath.Join(root, "missing")} {
		if got, err := filesystemObjects(p, ""); err == nil || got != nil {
			t.Fatalf("accepted invalid root %q: %v", p, err)
		}
	}
}

func TestFilesystemObjectsRefuseUnreadableSubtree(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read directories without permission")
	}
	root := t.TempDir()
	for i := range 32 {
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	for range 10 {
		if inventory, err := filesystemObjects(root, ""); err == nil || inventory != nil {
			t.Fatal("partial inventory accepted after directory read failure")
		}
	}
}
