package backup

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// CanLock says whether this system can lock outputs.
const CanLock = true

// lock takes the lock at path without waiting, and returns what releases it.
// The lock also goes when the process ends, however it ends, and git doesn't
// inherit it, since Go doesn't let child processes inherit handles.
func lock(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if err == nil {
		return func() {
			// Closing the file alone releases the lock only eventually.
			_ = windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
			_ = f.Close()
		}, nil
	}
	_ = f.Close()
	return nil, lockError(err)
}

// lockError says what an error from LockFileEx means for the run.
func lockError(err error) error {
	switch {
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return ErrLocked
	// Some network file systems don't support locks.
	case unsupported(err), errors.Is(err, windows.ERROR_INVALID_FUNCTION):
		return fmt.Errorf("%w: %w", errNoLocks, err)
	}
	return err
}
