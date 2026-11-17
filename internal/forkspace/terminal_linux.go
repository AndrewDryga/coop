//go:build linux

package forkspace

import "golang.org/x/sys/unix"

// ioctlReadTermios reads a terminal's attributes, and fails on anything that is not a terminal.
const ioctlReadTermios = unix.TCGETS
