//go:build !cooplivetest

package acpctl

import "os/exec"

func StartACPProcess(cmd *exec.Cmd, _ string) error { return cmd.Start() }
