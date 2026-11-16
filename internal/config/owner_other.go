//go:build !unix

package config

import "io/fs"

// ownedByMe reports false: there are no Unix owners to compare, so the key
// permission check is skipped. On Windows, OpenSSH checks the key's access
// list itself.
func ownedByMe(fs.FileInfo) bool { return false }
