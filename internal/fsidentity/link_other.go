//go:build !darwin && !linux

package fsidentity

import (
	"os"
	"path/filepath"
)

// Coop releases only for Darwin and Linux. Keep unsupported platforms buildable,
// but do not claim their path-based fallback has the same rename-race protection.
func linkRootNames(oldRoot *os.Root, oldName string, newRoot *os.Root, newName string) error {
	return os.Link(filepath.Join(oldRoot.Name(), oldName), filepath.Join(newRoot.Name(), newName))
}
