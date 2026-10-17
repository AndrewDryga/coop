//go:build linux

package forkspace

import "golang.org/x/sys/unix"

func renameNoReplace(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}

// renameAtNoReplace is renameNoReplace between names in two open folders.
func renameAtNoReplace(fromDir int, from string, toDir int, to string) error {
	return unix.Renameat2(fromDir, from, toDir, to, unix.RENAME_NOREPLACE)
}
