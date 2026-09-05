//go:build darwin

package preset

import "golang.org/x/sys/unix"

func renameScaffoldBundle(from, to int, name string) error {
	return unix.RenameatxNp(from, "bundle", to, name, unix.RENAME_EXCL)
}
