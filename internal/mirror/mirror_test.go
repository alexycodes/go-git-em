package mirror

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexycodes/go-git-em/internal/gitcmd"
	"github.com/alexycodes/go-git-em/internal/gittest"
)

// setup returns a fixture repository and a Mirror of it that doesn't exist yet.
func setup(t *testing.T) (git *gitcmd.Runner, src string, m *Mirror) {
	t.Helper()
	git = gittest.Runner(t)
	dir := t.TempDir()
	src = filepath.Join(dir, "src")
	gittest.Fixture(t, git, src)
	return git, src, &Mirror{Path: filepath.Join(dir, "out", "mirror.git"), Git: git}
}

func mustSync(t *testing.T, m *Mirror, input string) (oldURL string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	_, oldURL, err := m.Sync(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	return oldURL
}

func fingerprint(t *testing.T, m *Mirror) []string {
	t.Helper()
	refs, err := m.Fingerprint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestSyncClones(t *testing.T) {
	git, src, m := setup(t)
	if oldURL := mustSync(t, m, src); oldURL != "" {
		t.Errorf("first Sync: old URL = %q, want none", oldURL)
	}
	if bare := gittest.Git(t, git, m.Path, "rev-parse", "--is-bare-repository"); bare != "true" {
		t.Errorf("the mirror isn't bare")
	}
	if got, want := fingerprint(t, m), gittest.Refs(t, git, src); !slices.Equal(got, want) {
		t.Errorf("Fingerprint =\n%q\nwant the source's refs\n%q", got, want)
	}
	if _, err := os.Stat(m.Path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("mirror.git.tmp is left over: %v", err)
	}

	// No object file is shared with the source through a hard link.
	srcFiles := files(t, filepath.Join(src, ".git", "objects"))
	for _, f := range files(t, filepath.Join(m.Path, "objects")) {
		for _, s := range srcFiles {
			if os.SameFile(f, s) {
				t.Errorf("the mirror shares a file with the source")
			}
		}
	}
}

func files(t *testing.T, dir string) []fs.FileInfo {
	t.Helper()
	var infos []fs.FileInfo
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		infos = append(infos, info)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return infos
}

func TestSyncFollowsUpstream(t *testing.T) {
	git, src, m := setup(t)
	mustSync(t, m, src)

	// A new commit, a rewritten branch, a deleted branch and tag, a new tag.
	gittest.Commit(t, git, src, "two", map[string]string{"README": "hello again\n"})
	gittest.Git(t, git, src, "switch", "--quiet", "feature")
	gittest.Git(t, git, src, "reset", "--quiet", "--hard", "HEAD~1")
	gittest.Commit(t, git, src, "rewritten", map[string]string{"other.txt": "x\n"})
	gittest.Git(t, git, src, "switch", "--quiet", "main")
	gittest.Git(t, git, src, "branch", "--delete", "--force", "same-tip")
	gittest.Git(t, git, src, "tag", "--delete", "light")
	gittest.Git(t, git, src, "tag", "v2")

	if oldURL := mustSync(t, m, src); oldURL != "" {
		t.Errorf("old URL = %q, want none", oldURL)
	}
	if got, want := fingerprint(t, m), gittest.Refs(t, git, src); !slices.Equal(got, want) {
		t.Errorf("Fingerprint =\n%q\nwant the source's refs\n%q", got, want)
	}
}

func TestSyncNewURL(t *testing.T) {
	git, src, m := setup(t)
	mustSync(t, m, src)
	other := filepath.Join(t.TempDir(), "other.git")
	gittest.Git(t, git, "", "clone", "--quiet", "--bare", src, other)
	gittest.Commit(t, git, src, "only in src", nil)

	if oldURL := mustSync(t, m, other); oldURL != src {
		t.Errorf("old URL = %q, want %q", oldURL, src)
	}
	if url := gittest.Git(t, git, m.Path, "config", "--get", "remote.origin.url"); url != other {
		t.Errorf("origin = %q, want %q", url, other)
	}
	if got, want := fingerprint(t, m), gittest.Refs(t, git, other); !slices.Equal(got, want) {
		t.Errorf("Fingerprint =\n%q\nwant the new input's refs\n%q", got, want)
	}
	if oldURL := mustSync(t, m, other); oldURL != "" {
		t.Errorf("again: old URL = %q, want none", oldURL)
	}
}

func TestSyncLeftoverClone(t *testing.T) {
	git, src, m := setup(t)
	if err := os.MkdirAll(m.Path+".tmp/objects", 0o755); err != nil {
		t.Fatal(err)
	}
	mustSync(t, m, src)
	if _, err := os.Stat(m.Path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("mirror.git.tmp is left over: %v", err)
	}
	if got, want := fingerprint(t, m), gittest.Refs(t, git, src); !slices.Equal(got, want) {
		t.Errorf("Fingerprint =\n%q\nwant\n%q", got, want)
	}
}

func TestSyncFailedClone(t *testing.T) {
	_, src, m := setup(t)
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Sync(t.Context(), src+"-missing"); err == nil {
		t.Fatal("Sync of a missing input succeeded")
	}
	for _, path := range []string{m.Path, m.Path + ".tmp"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists after a failed clone: %v", filepath.Base(path), err)
		}
	}
}

func TestEmptyUpstream(t *testing.T) {
	git, _, m := setup(t)
	empty := filepath.Join(t.TempDir(), "empty.git")
	gittest.Git(t, git, "", "init", "--quiet", "--bare", empty)
	mustSync(t, m, empty)
	if refs := fingerprint(t, m); len(refs) != 0 {
		t.Errorf("Fingerprint = %q, want none", refs)
	}
	if lfs, err := m.UsesLFS(t.Context()); err != nil || lfs {
		t.Errorf("UsesLFS without a commit = %v, %v; want false", lfs, err)
	}
}

func TestUsesLFS(t *testing.T) {
	git, src, m := setup(t)
	mustSync(t, m, src)
	if lfs, err := m.UsesLFS(t.Context()); err != nil || lfs {
		t.Errorf("UsesLFS without .gitattributes = %v, %v; want false", lfs, err)
	}

	gittest.Commit(t, git, src, "attributes", map[string]string{".gitattributes": "*.txt text\n"})
	mustSync(t, m, src)
	if lfs, err := m.UsesLFS(t.Context()); err != nil || lfs {
		t.Errorf("UsesLFS without LFS = %v, %v; want false", lfs, err)
	}

	gittest.Commit(t, git, src, "lfs", map[string]string{".gitattributes": "*.txt text\n*.bin filter=lfs diff=lfs merge=lfs -text\n"})
	mustSync(t, m, src)
	if lfs, err := m.UsesLFS(t.Context()); err != nil || !lfs {
		t.Errorf("UsesLFS = %v, %v; want true", lfs, err)
	}
}

// git's background maintenance would escape the run's process group and
// timeout, so fetch and gc must keep it in the foreground.
func TestNoBackgroundMaintenance(t *testing.T) {
	git, src, m := setup(t)
	log := filepath.Join(t.TempDir(), "args")
	m.Git = gittest.FakeGit(t, git.Env, `echo "$@" >> '`+log+`'
exec "$GIT" "$@"`)

	mustSync(t, m, src) // clone
	mustSync(t, m, src) // fetch
	if err := m.Housekeep(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"fetch --prune --quiet --no-auto-maintenance origin",
		"-c gc.autoDetach=false gc --auto --quiet",
	} {
		if !strings.Contains(string(data), want+"\n") {
			t.Errorf("git didn't run %q; it ran:\n%s", want, data)
		}
	}
}

func TestFollowHead(t *testing.T) {
	// An upstream that is itself a clone also has refs/remotes/origin/HEAD,
	// which mustn't be taken for its HEAD.
	for _, cloned := range []bool{false, true} {
		t.Run(fmt.Sprintf("cloned upstream %v", cloned), func(t *testing.T) {
			git, src, m := setup(t)
			if cloned {
				clone := filepath.Join(t.TempDir(), "clone")
				gittest.Git(t, git, "", "clone", "--quiet", src, clone)
				src = clone
			}
			follow := func() string {
				t.Helper()
				mustSync(t, m, src)
				if err := m.FollowHead(t.Context()); err != nil {
					t.Fatal(err)
				}
				return gittest.Git(t, git, m.Path, "symbolic-ref", "HEAD")
			}
			follow()

			// Upstream renames its default branch...
			gittest.Git(t, git, src, "branch", "--move", "main", "trunk")
			if got := follow(); got != "refs/heads/trunk" {
				t.Errorf("after a rename: HEAD = %s, want refs/heads/trunk", got)
			}
			// ...then makes another existing branch the default.
			gittest.Git(t, git, src, "switch", "--quiet", "--create", "other")
			if got := follow(); got != "refs/heads/other" {
				t.Errorf("after a switch: HEAD = %s, want refs/heads/other", got)
			}
		})
	}
}

// A mirror that isn't a repository any more must fail, rather than let git
// find and change whatever repository contains the output.
func TestDamagedMirrorInsideRepository(t *testing.T) {
	git := gittest.Runner(t)
	parent := filepath.Join(t.TempDir(), "parent")
	gittest.Git(t, git, "", "init", "--quiet", parent)
	gittest.Git(t, git, parent, "remote", "add", "origin", "https://example.invalid/parent.git")
	m := &Mirror{Path: filepath.Join(parent, "out", "mirror.git"), Git: git}
	if err := os.MkdirAll(m.Path, 0o755); err != nil { // empty: not a repository
		t.Fatal(err)
	}

	if _, _, err := m.Sync(t.Context(), filepath.Join(t.TempDir(), "src")); err == nil {
		t.Error("Sync of a damaged mirror succeeded")
	}
	if _, err := m.Fingerprint(t.Context()); err == nil {
		t.Error("Fingerprint of a damaged mirror succeeded")
	}
	if err := m.Housekeep(t.Context()); err == nil {
		t.Error("Housekeep of a damaged mirror succeeded")
	}
	if url := gittest.Git(t, git, parent, "config", "--get", "remote.origin.url"); url != "https://example.invalid/parent.git" {
		t.Errorf("the parent repository's origin became %q", url)
	}
}

// A damaged object is an error, not "no LFS".
func TestUsesLFSDamaged(t *testing.T) {
	git := gittest.Runner(t)
	src := filepath.Join(t.TempDir(), "src")
	gittest.Git(t, git, "", "init", "--quiet", src)
	gittest.Commit(t, git, src, "lfs", map[string]string{".gitattributes": "*.bin filter=lfs\n"})
	oid := gittest.Git(t, git, src, "rev-parse", "HEAD:.gitattributes")
	if err := os.Remove(filepath.Join(src, ".git", "objects", oid[:2], oid[2:])); err != nil {
		t.Fatal(err)
	}
	m := &Mirror{Path: filepath.Join(src, ".git"), Git: git}
	if lfs, err := m.UsesLFS(t.Context()); err == nil {
		t.Errorf("UsesLFS = %v, nil; want an error", lfs)
	}
}

func TestUsesLFSParsing(t *testing.T) {
	tests := map[string]bool{
		"*.bin filter=lfs diff=lfs merge=lfs -text\n": true,
		"*.txt text\n*.psd   filter=lfs\n":            true,
		"# *.psd filter=lfs\n":                        false, // a comment
		"*.bin filter=lfs-custom\n":                   false,
		"filter=lfs\n":                                false, // a pattern with no attributes
		"":                                            false,
	}
	for attributes, want := range tests {
		if got := usesLFS(attributes); got != want {
			t.Errorf("usesLFS(%q) = %v, want %v", attributes, got, want)
		}
	}
}

// A damaged tree is an error too, not "no LFS".
func TestUsesLFSDamagedTree(t *testing.T) {
	git := gittest.Runner(t)
	src := filepath.Join(t.TempDir(), "src")
	gittest.Git(t, git, "", "init", "--quiet", src)
	gittest.Commit(t, git, src, "lfs", map[string]string{".gitattributes": "*.bin filter=lfs\n"})
	tree := gittest.Git(t, git, src, "rev-parse", "HEAD^{tree}")
	if err := os.Remove(filepath.Join(src, ".git", "objects", tree[:2], tree[2:])); err != nil {
		t.Fatal(err)
	}
	m := &Mirror{Path: filepath.Join(src, ".git"), Git: git}
	if lfs, err := m.UsesLFS(t.Context()); err == nil {
		t.Errorf("UsesLFS = %v, nil; want an error", lfs)
	}
}

func TestSyncReportsClone(t *testing.T) {
	_, src, m := setup(t)
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false} {
		cloned, _, err := m.Sync(t.Context(), src)
		if err != nil || cloned != want {
			t.Errorf("Sync %d: cloned = %v, %v; want %v", i+1, cloned, err, want)
		}
	}
}

// Upstream can switch to a branch it created after the fetch: HEAD waits for
// the next run rather than point at a branch the mirror doesn't have.
func TestFollowHeadUnfetchedBranch(t *testing.T) {
	git, src, m := setup(t)
	mustSync(t, m, src)
	gittest.Git(t, git, src, "switch", "--quiet", "--create", "brand-new")
	if err := m.FollowHead(t.Context()); err != nil { // no fetch since
		t.Fatal(err)
	}
	if head := gittest.Git(t, git, m.Path, "symbolic-ref", "HEAD"); head != "refs/heads/main" {
		t.Errorf("HEAD = %s, want it unchanged", head)
	}
}
