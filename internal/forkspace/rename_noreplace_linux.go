//go:build linux

package forkspace

import "golang.org/x/sys/unix"

// renameAtNoReplace renames from in fromDir to to in toDir, failing when to already exists.
func renameAtNoReplace(fromDir int, from string, toDir int, to string) error {
	return unix.Renameat2(fromDir, from, toDir, to, unix.RENAME_NOREPLACE)
}
