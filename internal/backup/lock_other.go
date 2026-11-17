//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package backup

// CanLock says whether this system can lock outputs. Without flock or
// LockFileEx it can't, and go-git-em says so once at startup.
const CanLock = false

// lock does nothing, without flock or LockFileEx.
func lock(string) (unlock func(), err error) { return func() {}, nil }
