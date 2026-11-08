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

// Every function takes repo, the repository the bundle comes from, and names
// it with --git-dir: otherwise git would use whichever repository contains
// the current directory, or the bundle.

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
// CheckPack checks the pack.
func Verify(ctx context.Context, git *gitcmd.Runner, repo, path string) error {
	if _, err := git.Run(ctx, "", "--git-dir="+repo, "bundle", "verify", "--quiet", path); err != nil {
		return err
	}
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
