//go:build darwin

package runtime

import "golang.org/x/sys/unix"

const ioctlReadTermios = unix.TIOCGETA
