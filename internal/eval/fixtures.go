package eval

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// manifestLimit bounds a suite manifest: it is a small hand-written file, and an unbounded read is
// a way to make a loader spend memory before it validates anything.
const manifestLimit = 1 << 20

// readManifest anchors the directory and the manifest to the same opened root. A later staging
// pass can reject a replaced suite root instead of freezing bytes from a path swapped to the
// operator's private files.
func readManifest(path string) ([]byte, os.FileInfo, os.FileInfo, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("suite %q: %w", path, err)
	}
	defer root.Close()
	dirInfo, err := root.Stat(".")
	if err != nil {
		return nil, nil, nil, err
	}
	data, fileInfo, err := readManifestFromRoot(root, filepath.Base(path))
	return data, dirInfo, fileInfo, err
}

func readManifestFromRoot(root *os.Root, name string) ([]byte, os.FileInfo, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, nil, fmt.Errorf("suite %q: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("suite %q is a symbolic link; name the file itself", name)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("suite %q is not a regular file", name)
	}
	if info.Size() > manifestLimit {
		return nil, nil, fmt.Errorf("suite %q is larger than %d bytes", name, manifestLimit)
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("suite %q changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, manifestLimit+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > manifestLimit {
		return nil, nil, fmt.Errorf("suite %q is larger than %d bytes", name, manifestLimit)
	}
	return data, opened, nil
}

// refuseSymlink rejects a path — or ANY directory component between the suite root and it — that is
// a symlink. A suite's inputs must be real files that travel with it; a link at any level (the leaf,
// or a parent like `verifiers` pointing outside) is a way to reach a grader or a credential outside
// the tree the manifest is allowed to name. A component that does not exist yet is not refused here
// (existence is a preparation concern), but every component that DOES exist is checked. This is a
// LOAD-time, lexical-plus-Lstat check: a link created between load and mount is not caught here, so
// preparation (a later milestone) must re-run it on the resolved tree before anything is mounted.
//
// root is the suite directory (relPath has already proved full is within it); the walk stops there,
// so it is bounded regardless of how deep full is.
func refuseSymlink(field, root, full string) error {
	for cur := full; cur != root; cur = filepath.Dir(cur) {
		info, err := os.Lstat(cur)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s passes through a symbolic link at %q; use real files inside the suite", field, cur)
		}
		if parent := filepath.Dir(cur); parent == cur {
			break // reached the filesystem root without meeting the suite dir — should not happen
		}
	}
	return nil
}
