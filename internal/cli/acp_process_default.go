//go:build !cooplivetest

package cli

import "os/exec"

func startACPProcess(cmd *exec.Cmd, _ string) error { return cmd.Start() }
