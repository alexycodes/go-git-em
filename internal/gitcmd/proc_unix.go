//go:build unix

package gitcmd

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// group is git's process group: git and the helpers it starts.
type group struct{ cmd *exec.Cmd }

// isolate starts cmd in a new session. git and every helper it starts then
// share one process group that can be signalled as a whole, and none of them
// has a controlling terminal: they can't prompt on it, and a Ctrl-C typed
// there doesn't reach them. When the context is done, the group gets SIGTERM,
// on which git removes its lock files.
func isolate(cmd *exec.Cmd) *group {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return &group{cmd}
}

// started has nothing to do: the group exists as soon as git does.
func (*group) started() error { return nil }

// kill kills whatever is left of the group after git exited. The group's ID
// stays reserved while any member is alive, so the signal only reaches git's
// own helpers. With none left, the call fails with ESRCH.
func (g *group) kill() {
	_ = syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL)
}
