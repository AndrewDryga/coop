package main

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal: the bench's side as a raw descriptor it selects on, and the
// terminal coop runs on.
func openPTY() (int, *os.File, error) {
	master, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, nil, err
	}
	number, err := unix.IoctlGetUint32(master, unix.TIOCGPTN) // ptsname
	if err == nil {
		err = unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, 0) // unlockpt
	}
	var tty *os.File
	if err == nil {
		tty, err = os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(number), 10), os.O_RDWR|unix.O_NOCTTY, 0)
	}
	if err != nil {
		_ = unix.Close(master)
		return -1, nil, err
	}
	return master, tty, nil
}
