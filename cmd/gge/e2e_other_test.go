//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// gge is the gge command in scripts.
func gge() { main() }

// stop sends SIGTERM to cmd, running in the background, as docker stop and
// systemd do.
func stop(cmd *exec.Cmd) error {
	return cmd.Process.Signal(syscall.SIGTERM)
}
