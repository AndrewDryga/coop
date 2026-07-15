//go:build !cooplivetest

package runtime

import "syscall"

func interruptibleProcessEnvironment() []string { return nil }

func beforeInterruptibleCancel() error { return nil }

func interruptibleProcessGroup() (*syscall.SysProcAttr, error) {
	return &syscall.SysProcAttr{Setpgid: true}, nil
}
