//go:build !unix

package runner

import "os/exec"

func killGroup(cmd *exec.Cmd) { cmd.WaitDelay = WaitDelay }
