//go:build boxruntimee2e

package box

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The locked image runs as node. Make only synthetic bind contents readable after all writes,
// including under umask 077; t.TempDir's enclosing host directory stays private.
func readableLockedClientFixture(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		mode := fs.FileMode(0o644)
		if entry.IsDir() {
			mode = 0o755
		} else if !entry.Type().IsRegular() {
			return fmt.Errorf("non-regular locked-client fixture %s", path)
		} else {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode()&0o100 != 0 {
				mode = 0o755
			}
		}
		return os.Chmod(path, mode)
	})
	if err != nil {
		t.Fatal(err)
	}
}
