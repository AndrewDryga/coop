//go:build darwin || linux

package fsidentity

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// linkRootNames links two names relative to already-open directory handles. Path-based
// os.Link would reopen both roots by name and let an attacker swap a writable ancestor
// between validation and link creation.
func linkRootNames(oldRoot *os.Root, oldName string, newRoot *os.Root, newName string) error {
	oldDir, err := oldRoot.Open(".")
	if err != nil {
		return err
	}
	newDir, err := newRoot.Open(".")
	if err != nil {
		return errors.Join(err, oldDir.Close())
	}
	linkErr := unix.Linkat(int(oldDir.Fd()), oldName, int(newDir.Fd()), newName, 0)
	return errors.Join(linkErr, newDir.Close(), oldDir.Close())
}
