//go:build !unix && !windows

package gitcmd

import "os/exec"

// group does nothing on systems without process groups.
type group struct{}

// isolate keeps exec's default: when the context is done, only git itself is
// killed.
func isolate(*exec.Cmd) *group { return &group{} }

func (*group) started() error { return nil }

func (*group) kill() {}
