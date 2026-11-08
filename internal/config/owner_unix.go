//go:build unix

package config

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByMe reports whether the current user owns the file. OpenSSH checks a
// key's permissions only then: it uses a key that another user owns, such as
// one mounted read-only and owned by root, whatever its mode.
func ownedByMe(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
