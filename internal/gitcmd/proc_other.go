//go:build !unix

package gitcmd

import "os/exec"

// isolate keeps exec's default on systems without process groups: when the
// context is done, only git itself is killed. Windows is not supported yet.
func isolate(*exec.Cmd) {}

// killGroup has nothing to do without process groups.
func killGroup(*exec.Cmd) {}
