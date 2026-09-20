package eval

import (
	"fmt"
	"os"
	"path/filepath"
)

// manifestLimit bounds a suite manifest: it is a small hand-written file, and an unbounded read is
// a way to make a loader spend memory before it validates anything.
const manifestLimit = 1 << 20

// readManifest reads a suite manifest as a plain regular file — never following a symlink at the
// manifest itself — and bounds its size. A missing or non-regular manifest is refused by name.
func readManifest(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("suite %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("suite %q is a symbolic link; name the file itself", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("suite %q is not a regular file", path)
	}
	if info.Size() > manifestLimit {
		return nil, fmt.Errorf("suite %q is larger than %d bytes", path, manifestLimit)
	}
	return os.ReadFile(path)
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
