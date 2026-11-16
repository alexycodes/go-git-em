//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package backup

import (
	"syscall"
	"testing"
)

func TestLockError(t *testing.T) {
	for _, tt := range []struct {
		errno syscall.Errno
		want  error
	}{
		{syscall.EWOULDBLOCK, ErrLocked},
		{syscall.EACCES, ErrLocked},
		{syscall.ENOLCK, errNoLocks},
		{syscall.ENOTSUP, errNoLocks},
		{syscall.EINVAL, errNoLocks},
		{syscall.EIO, syscall.EIO},
	} {
		checkLockError(t, tt.errno, tt.want)
	}
}
