// Package bundle creates and checks Git bundles, the snapshot files.
package bundle

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/alexycodes/go-git-em/internal/gitcmd"
)

// Create, Heads and Verify take repo, the repository the bundle comes from,
// and name it with --git-dir: otherwise git would use whichever repository
// contains the current directory, or the bundle. Restore needs none: git
// clone uses no repository but the one it creates.

// Create writes a bundle of every ref in repo to path, and of HEAD if it
// points at a commit, so that git clone of the bundle checks out the default
// branch.
func Create(ctx context.Context, git *gitcmd.Runner, repo, path string) error {
	_, err := git.Run(ctx, "", "--git-dir="+repo, "bundle", "create", "--quiet", path, "--all")
	return err
}

// Heads returns the refs in a bundle, and HEAD if it has it, as sorted
// "objectname refname" lines: the form of mirror.Fingerprint. It reads only
// the bundle's header.
func Heads(ctx context.Context, git *gitcmd.Runner, repo, path string) ([]string, error) {
	out, err := git.Run(ctx, "", "--git-dir="+repo, "bundle", "list-heads", path)
	if err != nil {
		return nil, err
	}
	var heads []string
	for line := range strings.Lines(string(out)) {
		heads = append(heads, strings.TrimSuffix(line, "\n"))
	}
	slices.Sort(heads)
	return heads, nil
}

// Verify checks a whole bundle: git bundle verify checks the header, then
// CheckFile checks the pack.
func Verify(ctx context.Context, git *gitcmd.Runner, repo, path string) error {
	if _, err := git.Run(ctx, "", "--git-dir="+repo, "bundle", "verify", "--quiet", path); err != nil {
		return err
	}
	return CheckFile(ctx, path)
}

// CheckFile runs CheckPack on the bundle at path.
func CheckFile(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = CheckPack(ctxReader{ctx, f})
	_ = f.Close() // opened read-only: nothing to lose
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ctxReader stops reading when ctx is done, so the run's timeout also covers
// reading a large pack.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Restore clones the bundle at path into dir, which mustn't exist yet, as a
// bare mirror. That checks what Verify can't: git hashes every object, and
// checks that every ref reaches only objects the bundle holds. A clone
// without --mirror would check only branches and tags.
func Restore(ctx context.Context, git *gitcmd.Runner, path, dir string) error {
	// A policy that allows only network protocols, in protocol.allow or
	// GIT_ALLOW_PROTOCOL, would refuse a local bundle. This clone needs only
	// the file transport, and the variable overrides the config.
	_, err := git.WithEnv("GIT_ALLOW_PROTOCOL=file").Run(ctx, "",
		// Callers delete dir right away: unflushed, it may never reach the
		// disk. git before 2.36 doesn't know the setting, and ignores it.
		"-c", "core.fsync=none",
		// No hint about naming the first branch, which would fill an error.
		"-c", "init.defaultBranch=main",
		"clone", "--mirror", "--quiet", "--", path, dir)
	return err
}
