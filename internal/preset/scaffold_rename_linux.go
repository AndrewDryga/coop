//go:build linux

package preset

import "golang.org/x/sys/unix"

func renameScaffoldBundle(from, to int, name string) error {
	return unix.Renameat2(from, "bundle", to, name, unix.RENAME_NOREPLACE)
}
