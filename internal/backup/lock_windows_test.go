package backup

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestLockError(t *testing.T) {
	for _, tt := range []struct {
		errno windows.Errno
		want  error
	}{
		{windows.ERROR_LOCK_VIOLATION, ErrLocked},
		{windows.ERROR_NOT_SUPPORTED, errNoLocks},
		{windows.ERROR_INVALID_FUNCTION, errNoLocks},
		{windows.ERROR_ACCESS_DENIED, windows.ERROR_ACCESS_DENIED},
	} {
		checkLockError(t, tt.errno, tt.want)
	}
}
