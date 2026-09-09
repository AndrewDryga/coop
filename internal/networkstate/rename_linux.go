package networkstate

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameExclusive(dir *os.File, from, to string) error {
	return unix.Renameat2(int(dir.Fd()), from, int(dir.Fd()), to, unix.RENAME_NOREPLACE)
}
