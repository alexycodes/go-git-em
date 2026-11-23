// Package gittest sets up git for tests: an environment that ignores the
// user's and the system's git configuration, and fixture repositories.
package gittest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/alexycodes/go-git-em/internal/gitcmd"
)

// config is the global git config of every test.
const config = `[user]
	name = go-git-em tests
	email = tests@go-git-em.invalid
[init]
	defaultBranch = main
[protocol "file"]
	allow = always
`

// Env returns an environment for git that ignores the user's and the
// system's git configuration: a fresh HOME, and a global config with a fixed
// identity and default branch. PATH is the test process's.
func Env(t testing.TB) []string {
	t.Helper()
	home := t.TempDir()
	global := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(global, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + global,
	}
}

// Runner returns a Runner that uses Env.
func Runner(t testing.TB) *gitcmd.Runner {
	t.Helper()
	return &gitcmd.Runner{Env: Env(t)}
}

// Git runs git in dir and returns its output without trailing white space.
// It fails the test if git fails.
func Git(t testing.TB, git *gitcmd.Runner, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(t.Context(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(out), "\n")
}

// Commit writes files into the work tree at dir, then commits them with msg.
func Commit(t testing.TB, git *gitcmd.Runner, dir, msg string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Git(t, git, dir, "add", "--all")
	Git(t, git, dir, "commit", "--quiet", "--allow-empty", "--message", msg)
}

// Fixture creates a work tree repository at dir with what a backup must keep:
// several branches, two of them on the same commit, an annotated and a
// lightweight tag, and a binary file.
func Fixture(t testing.TB, git *gitcmd.Runner, dir string) {
	t.Helper()
	Git(t, git, "", "init", "--quiet", dir)
	Commit(t, git, dir, "first", map[string]string{"README": "hello\n"})
	Git(t, git, dir, "tag", "--annotate", "--message", "version 1", "v1")
	Commit(t, git, dir, "binary", map[string]string{"data.bin": "\x00\x01\x02\xff\xfe binary \x00"})
	Git(t, git, dir, "branch", "same-tip")
	Git(t, git, dir, "switch", "--quiet", "--create", "feature")
	Commit(t, git, dir, "feature", map[string]string{"src/feature.txt": "feature\n"})
	Git(t, git, dir, "tag", "light")
	Git(t, git, dir, "switch", "--quiet", "main")
}

// Refs returns the refs of the repository at dir, and HEAD if it points at a
// commit, as sorted "objectname refname" lines: the form of a mirror's
// fingerprint.
func Refs(t testing.TB, git *gitcmd.Runner, dir string) []string {
	t.Helper()
	out, err := git.Run(t.Context(), dir, "show-ref", "--head")
	if gitcmd.ExitCode(err) == 1 {
		return nil // no refs
	}
	if err != nil {
		t.Fatal(err)
	}
	refs := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	slices.Sort(refs)
	return refs
}

// DamageObject flips 4 bytes in the middle of an object's data, in a pack of
// the bare repository at repo, as a failing disk might. object is anything
// rev-parse takes, such as HEAD:README. It returns an error rather than fail
// a test, for the scripts' damage command.
func DamageObject(ctx context.Context, git *gitcmd.Runner, repo, object string) error {
	out, err := git.Run(ctx, repo, "rev-parse", "--verify", object)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(out))
	dir := filepath.Join(repo, "objects", "pack")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		pack, ok := strings.CutSuffix(e.Name(), ".idx")
		if !ok {
			continue
		}
		out, err := git.Run(ctx, repo, "verify-pack", "--verbose", filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		for line := range strings.Lines(string(out)) {
			// Object name, type, size, size in the pack, offset, and for a
			// delta its depth and base.
			f := strings.Fields(line)
			if len(f) < 5 || f[0] != id {
				continue
			}
			size, err1 := strconv.ParseInt(f[3], 10, 64)
			offset, err2 := strconv.ParseInt(f[4], 10, 64)
			if err := errors.Join(err1, err2); err != nil {
				return err
			}
			return flip(filepath.Join(dir, pack+".pack"), offset+size/2-2, 4)
		}
	}
	return fmt.Errorf("%s isn't in a pack of %s", object, repo)
}

// flip inverts n bytes of the file at path, from at.
func flip(path string, at int64, n int) error {
	if err := os.Chmod(path, 0o644); err != nil { // git writes packs read-only
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	buf := make([]byte, n)
	_, err = f.ReadAt(buf, at)
	if err == nil {
		for i := range buf {
			buf[i] ^= 0xff
		}
		_, err = f.WriteAt(buf, at)
	}
	return errors.Join(err, f.Close())
}
