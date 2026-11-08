package gittest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alexycodes/go-git-em/internal/gitcmd"
)

// FakeGit returns a Runner whose git is a sh script, for tests that change
// what git does or record how it's called. The script gets the real git, from
// PATH in env, as $GIT.
func FakeGit(t testing.TB, env []string, script string) *gitcmd.Runner {
	t.Helper()
	// The script's own directory isn't on PATH, so command -v finds the real git.
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nGIT=$(command -v git) || exit\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &gitcmd.Runner{Path: path, Env: env}
}
