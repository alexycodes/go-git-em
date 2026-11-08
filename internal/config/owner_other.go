//go:build !unix

package config

import "io/fs"

// ownedByMe reports false: there are no Unix owners to compare, so the key
// permission check is skipped. Windows isn't supported yet.
func ownedByMe(fs.FileInfo) bool { return false }
