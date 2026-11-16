//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package backup

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// CanLock says whether this system can lock outputs.
const CanLock = true

// lock takes the lock at path without waiting, and returns what releases it.
// The lock also goes when the process ends, however it ends, and git doesn't
// inherit it, since Go opens files close-on-exec.
func lock(path string) (unlock func(), err error) {
	// Writable: Linux emulates flock on NFS with a write lock.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // anyone who can open it can hold it
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) { // a signal can cut an NFS lock request short
			break
		}
	}
	if err == nil {
		return func() { _ = f.Close() }, nil
	}
	_ = f.Close()
	return nil, lockError(err)
}

// lockError says what an error from flock means for the run.
func lockError(err error) error {
	switch {
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EACCES): // SMB says EACCES
		return ErrLocked
	// ENOLCK: NFS without a lock service. Some network and FUSE file systems
	// don't support locks at all.
	case unsupported(err), errors.Is(err, syscall.ENOLCK):
		return fmt.Errorf("%w: %w", errNoLocks, err)
	}
	return err
}
