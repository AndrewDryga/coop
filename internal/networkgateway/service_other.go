//go:build !linux

package networkgateway

import "syscall"

func envoyProcessAttributes() *syscall.SysProcAttr { return nil }
func verifyServiceRole(string) error               { return Failure("unsupported_capability") }
