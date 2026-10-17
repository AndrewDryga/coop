//go:build darwin

package forkspace

import "golang.org/x/sys/unix"

func renameNoReplace(from, to string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_EXCL)
}

// renameAtNoReplace is renameNoReplace between names in two open folders.
func renameAtNoReplace(fromDir int, from string, toDir int, to string) error {
	return unix.RenameatxNp(fromDir, from, toDir, to, unix.RENAME_EXCL)
}
