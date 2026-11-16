//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package backup

// CanLock says whether this system can lock outputs. Without flock it can't,
// and go-git-em says so once at startup.
const CanLock = false

// lock does nothing, without flock.
func lock(string) (unlock func(), err error) { return func() {}, nil }
