//go:build linux

package runtime

import "golang.org/x/sys/unix"

const ioctlReadTermios = unix.TCGETS
