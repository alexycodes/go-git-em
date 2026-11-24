// Package mirror keeps a bare mirror of a repository, from which snapshots
// are taken.
package mirror

import (
	"compress/zlib"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alexycodes/go-git-em/internal/gitcmd"
)

// Mirror is a bare mirror repository at Path.
type Mirror struct {
	Path string
	Git  *gitcmd.Runner
}

// run runs git on the mirror. Naming it with --git-dir stops git from looking
// further up if the mirror is damaged, which would run the command on
// whatever repository contains the output.
func (m *Mirror) run(ctx context.Context, args ...string) ([]byte, error) {
	return m.Git.Run(ctx, "", append([]string{"--git-dir=" + m.Path}, args...)...)
}

// Sync clones input into the mirror the first time, and fetches from it
// afterwards, which it reports with cloned. If the mirror's origin was another
// URL, Sync points it at input and returns the old URL; otherwise it returns "".
func (m *Mirror) Sync(ctx context.Context, input string) (cloned bool, oldURL string, err error) {
	if _, err := os.Stat(m.Path); errors.Is(err, fs.ErrNotExist) {
		return true, "", m.clone(ctx, input)
	} else if err != nil {
		return false, "", err
	}

	out, err := m.run(ctx, "config", "--get", "remote.origin.url")
	if err != nil {
		return false, "", err
	}
	if url := strings.TrimSpace(string(out)); url != input {
		if _, err := m.run(ctx, "remote", "set-url", "origin", "--", input); err != nil {
			return false, "", err
		}
		oldURL = url
	}
	// The mirror's refspec, +refs/*:refs/*, follows force-pushes, and --prune
	// drops refs deleted upstream. Background maintenance would leave the
	// run's process group and outlive its timeout: Housekeep runs it instead.
	_, err = m.run(ctx, "fetch", "--prune", "--quiet", "--no-auto-maintenance", "origin")
	return false, oldURL, err
}

// clone clones into a temporary directory first, so that an interrupted
// clone never looks like a mirror.
func (m *Mirror) clone(ctx context.Context, input string) error {
	tmp := m.Path + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	// --no-local: copy a local input's objects rather than hard-link them,
	// so the backup shares no files with the original.
	if _, err := m.Git.Run(ctx, "", "clone", "--mirror", "--no-local", "--quiet", "--", input, tmp); err != nil {
		_ = os.RemoveAll(tmp) // the next run would remove it anyway
		return err
	}
	return os.Rename(tmp, m.Path)
}

// FollowHead points HEAD at upstream's default branch, which fetch doesn't
// update. Otherwise, after upstream renames its default branch, snapshots
// would restore without checking anything out.
func (m *Mirror) FollowHead(ctx context.Context) error {
	out, err := m.run(ctx, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return err
	}
	// The pattern also matches refs ending in /HEAD, such as
	// refs/remotes/origin/HEAD: only "ref: <branch>\tHEAD" is upstream's own.
	var branch string
	for line := range strings.Lines(string(out)) {
		if ref, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "ref: "); ok {
			if b, ok := strings.CutSuffix(ref, "\tHEAD"); ok {
				branch = b
				break
			}
		}
	}
	if !strings.HasPrefix(branch, "refs/heads/") {
		return nil // upstream is empty, or its HEAD isn't on a branch
	}
	// A branch created since the fetch: the next run will have it.
	if _, err := m.run(ctx, "show-ref", "--verify", "--quiet", branch); gitcmd.ExitCode(err) == 1 {
		return nil
	} else if err != nil {
		return err
	}
	if current, err := m.run(ctx, "symbolic-ref", "--quiet", "HEAD"); err == nil && strings.TrimSpace(string(current)) == branch {
		return nil
	}
	_, err = m.run(ctx, "symbolic-ref", "HEAD", branch)
	return err
}

// Fingerprint returns the mirror's refs, and HEAD if it points at a commit, as
// sorted "objectname refname" lines: the form of bundle.Heads, so that a
// snapshot can be compared with the mirror. It's empty without refs.
func (m *Mirror) Fingerprint(ctx context.Context) ([]string, error) {
	out, err := m.run(ctx, "show-ref", "--head")
	if gitcmd.ExitCode(err) == 1 {
		return nil, nil // no refs
	}
	if err != nil {
		return nil, err
	}
	var refs []string
	for line := range strings.Lines(string(out)) {
		refs = append(refs, strings.TrimSuffix(line, "\n"))
	}
	slices.Sort(refs)
	return refs, nil
}

// UsesLFS reports whether the default branch's .gitattributes uses Git LFS.
// Without a default branch or the file, it doesn't.
func (m *Mirror) UsesLFS(ctx context.Context) (bool, error) {
	if _, err := m.run(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); gitcmd.ExitCode(err) == 1 {
		return false, nil // no default branch
	} else if err != nil {
		return false, err
	}
	// ls-tree, unlike rev-parse HEAD:.gitattributes, tells a missing file from
	// a damaged tree.
	out, err := m.run(ctx, "ls-tree", "HEAD", "--", ".gitattributes")
	if err != nil {
		return false, err
	}
	fields := strings.Fields(string(out)) // mode, type, object name, path
	if len(fields) < 3 || fields[1] != "blob" {
		return false, nil
	}
	blob, err := m.run(ctx, "cat-file", "blob", fields[2])
	if err != nil {
		return false, err
	}
	return usesLFS(string(blob)), nil
}

// usesLFS reports whether a .gitattributes file gives any pattern the
// attribute filter=lfs.
func usesLFS(attributes string) bool {
	for line := range strings.Lines(attributes) {
		fields := strings.Fields(line)
		if len(fields) > 1 && !strings.HasPrefix(fields[0], "#") && slices.Contains(fields[1:], "filter=lfs") {
			return true
		}
	}
	return false
}

// Housekeep runs git's own maintenance if git thinks it's due (gc --auto).
// It runs in the foreground: by default git would detach it from the run's
// process group, beyond the run's timeout.
func (m *Mirror) Housekeep(ctx context.Context) error {
	_, err := m.run(ctx, "-c", "gc.autoDetach=false", "gc", "--auto", "--quiet")
	return err
}

// ErrDamaged means Verify found a loose object that doesn't inflate, or
// doesn't hash to its name.
var ErrDamaged = errors.New("damaged")

// Verify checks the mirror's objects: its packs with git verify-pack, which
// hashes every object in them, then its loose objects, which a fetch of fewer
// than transfer.unpackLimit (100) objects leaves until gc packs them.
func (m *Mirror) Verify(ctx context.Context) error {
	dir := filepath.Join(m.Path, "objects", "pack")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".idx") {
			if _, err := m.run(ctx, "verify-pack", filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return m.verifyLoose(ctx)
}

// verifyLoose checks that each loose object inflates, and hashes to its name.
// git itself checks neither when it reads one.
func (m *Mirror) verifyLoose(ctx context.Context) error {
	objects := filepath.Join(m.Path, "objects")
	dirs, err := os.ReadDir(objects)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if !d.IsDir() || len(d.Name()) != 2 || !isHex(d.Name()) { // info, pack
			continue
		}
		files, err := os.ReadDir(filepath.Join(objects, d.Name()))
		if err != nil {
			return err
		}
		for _, f := range files {
			name := d.Name() + f.Name()
			if !isHex(name) || len(name) != sha1.Size*2 && len(name) != sha256.Size*2 { // tmp_obj_*
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := checkLoose(filepath.Join(objects, d.Name(), f.Name()), name); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkLoose checks the loose object at path, named name. A damaged one is an
// ErrDamaged, and one that can't be read an *fs.PathError.
func checkLoose(path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	h := sha1.New()
	if len(name) == sha256.Size*2 { // a SHA-256 repository's
		h = sha256.New()
	}
	z, err := zlib.NewReader(f)
	if err == nil {
		_, err = io.Copy(h, z) // zlib checks its own checksum at the end
	}
	_ = f.Close() // opened read-only: nothing to lose
	if _, readErr := errors.AsType[*fs.PathError](err); readErr {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: loose object %s: %w", ErrDamaged, name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != name {
		return fmt.Errorf("%w: loose object %s hashes to %s", ErrDamaged, name, got)
	}
	return nil
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// RemoveStaleLocks removes the lock files last changed before cutoff, which a
// killed git left in the mirror: *.lock files, which make every later git
// refuse to run, and a fetch's .keep files, which hold its pack out of gc. No
// other process may be using the mirror. It returns those it removed,
// relative to the mirror, and carries on past errors.
func (m *Mirror) RemoveStaleLocks(cutoff time.Time) ([]string, error) {
	var removed []string
	var errs []error
	root := m.Path + string(filepath.Separator) // follows a mirror that's a symlink
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != root || !errors.Is(err, fs.ErrNotExist) { // else no mirror yet
				errs = append(errs, err)
			}
			return nil
		}
		rel, err := filepath.Rel(m.Path, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir() && path.Dir(rel) == "objects" && len(d.Name()) == 2:
			return fs.SkipDir // loose objects: git keeps no locks there
		case !d.Type().IsRegular() || !isLock(rel, p):
			return nil
		}
		switch info, err := d.Info(); {
		case errors.Is(err, fs.ErrNotExist): // removed meanwhile
			return nil
		case err != nil:
			errs = append(errs, err)
			return nil
		case !info.ModTime().Before(cutoff):
			return nil
		}
		if err := os.Remove(p); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			return nil
		}
		removed = append(removed, rel)
		return nil
	})
	return removed, errors.Join(append(errs, err)...)
}

// isLock reports whether the file at rel, relative to the mirror, is one of
// git's locks. git forbids refs whose names end in ".lock". A fetch's .keep
// file says "fetch-pack", unlike one a person might write.
func isLock(rel, file string) bool {
	if strings.HasSuffix(rel, ".lock") {
		return true
	}
	if !strings.HasPrefix(rel, "objects/pack/") || !strings.HasSuffix(rel, ".keep") {
		return false
	}
	data, err := os.ReadFile(file)
	return err == nil && strings.HasPrefix(string(data), "fetch-pack ")
}
