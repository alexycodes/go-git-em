// Package shtest runs sh scripts as programs in tests, such as a fake git or
// ssh, on Windows too.
package shtest

import (
	"os"
	"path/filepath"
	"testing"
)

// Script writes body as the sh script dir/name and returns the path to run it
// by: the script itself, or on Windows, which can't run a script, a program
// that runs it with Git for Windows' sh.
func Script(t testing.TB, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!"+interpreter(t)+"\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return program(t, path)
}
