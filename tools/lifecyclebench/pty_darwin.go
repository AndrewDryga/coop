package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal: the bench's side as a raw descriptor it selects on, and the
// terminal coop runs on.
func openPTY() (int, *os.File, error) {
	master, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, nil, err
	}
	err = unix.IoctlSetInt(master, unix.TIOCPTYGRANT, 0) // grantpt
	if err == nil {
		err = unix.IoctlSetInt(master, unix.TIOCPTYUNLK, 0) // unlockpt
	}
	// ptsname. The kernel names the terminal after the master's minor number, as its TIOCPTYGNAME
	// does; that ioctl fills a buffer, which x/sys/unix wraps for no request on macOS.
	var device unix.Stat_t
	if err == nil {
		err = unix.Fstat(master, &device)
	}
	var tty *os.File
	if err == nil {
		tty, err = os.OpenFile(fmt.Sprintf("/dev/ttys%03d", unix.Minor(uint64(device.Rdev))), os.O_RDWR|unix.O_NOCTTY, 0)
	}
	if err != nil {
		_ = unix.Close(master)
		return -1, nil, err
	}
	return master, tty, nil
}
