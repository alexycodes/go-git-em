package bundle

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexycodes/go-git-em/internal/gitcmd"
	"github.com/alexycodes/go-git-em/internal/gittest"
)

// mirrorOf returns a bare mirror of a fixture repository.
func mirrorOf(t *testing.T, git *gitcmd.Runner) string {
	t.Helper()
	dir := t.TempDir()
	gittest.Fixture(t, git, filepath.Join(dir, "src"))
	gittest.Git(t, git, dir, "clone", "--quiet", "--mirror", "src", "mirror.git")
	return filepath.Join(dir, "mirror.git")
}

func TestCreateHeadsVerify(t *testing.T) {
	git := gittest.Runner(t)
	repo := mirrorOf(t, git)
	path := filepath.Join(t.TempDir(), "snapshot.bundle")

	if err := Create(t.Context(), git, repo, path); err != nil {
		t.Fatal(err)
	}
	heads, err := Heads(t.Context(), git, repo, path)
	if err != nil {
		t.Fatal(err)
	}
	if want := gittest.Refs(t, git, repo); !slices.Equal(heads, want) {
		t.Errorf("Heads =\n%q\nwant the repository's refs and HEAD\n%q", heads, want)
	}
	if err := Verify(t.Context(), git, repo, path); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestVerifyOtherFormats(t *testing.T) {
	git := gittest.Runner(t)
	dir := t.TempDir()

	gittest.Git(t, git, dir, "init", "--quiet", "--object-format=sha256", "sha256")
	gittest.Commit(t, git, filepath.Join(dir, "sha256"), "one", map[string]string{"a": "a\n"})
	sha1Repo := mirrorOf(t, git)

	bundles := map[string][]string{ // bundle → repository, then git bundle create's options
		"sha256": {filepath.Join(dir, "sha256", ".git")},
		"v3":     {sha1Repo, "--version=3"},
	}
	for name, args := range bundles {
		path := filepath.Join(dir, name+".bundle")
		gittest.Git(t, git, args[0], append([]string{"bundle", "create", "--quiet"}, append(args[1:], path, "--all")...)...)
		if err := Verify(t.Context(), git, args[0], path); err != nil {
			t.Errorf("%s: Verify: %v", name, err)
		}
	}
}

func TestVerifyCatchesDamage(t *testing.T) {
	git := gittest.Runner(t)
	repo := mirrorOf(t, git)
	dir := t.TempDir()
	good := filepath.Join(dir, "good.bundle")
	if err := Create(t.Context(), git, repo, good); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}

	flipped := slices.Clone(data)
	for i := len(flipped) - 30; i < len(flipped)-26; i++ { // in the pack, before its checksum
		flipped[i] ^= 0xff
	}
	damaged := map[string][]byte{
		"flipped": flipped,
		"halved":  data[:len(data)/2],
	}
	for name, content := range damaged {
		path := filepath.Join(dir, name+".bundle")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		// git bundle verify reads only the header, which is why Verify has
		// CheckPack. Logged rather than checked: a later git may do better.
		_, gitErr := git.Run(t.Context(), repo, "bundle", "verify", "--quiet", path)
		t.Logf("%s: git bundle verify alone: %v", name, gitErr)

		if err := Verify(t.Context(), git, repo, path); err == nil {
			t.Errorf("%s: Verify accepted a damaged bundle", name)
		}
	}
}

func TestCheckFileCancelled(t *testing.T) {
	git := gittest.Runner(t)
	repo := mirrorOf(t, git)
	path := filepath.Join(t.TempDir(), "snapshot.bundle")
	if err := Create(t.Context(), git, repo, path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := CheckFile(ctx, path); !errors.Is(err, context.Canceled) {
		t.Errorf("CheckFile = %v, want context.Canceled", err)
	}
}

func TestRestore(t *testing.T) {
	git := gittest.Runner(t)
	repo := mirrorOf(t, git)
	path := filepath.Join(t.TempDir(), "snapshot.bundle")
	if err := Create(t.Context(), git, repo, path); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restored.git")
	// A host that allows only network protocols: the mirror's fetch works,
	// and the test restore must too.
	if err := Restore(t.Context(), git.WithEnv("GIT_ALLOW_PROTOCOL=https:ssh"), path, dir); err != nil {
		t.Fatal(err)
	}
	if got, want := gittest.Refs(t, git, dir), gittest.Refs(t, git, repo); !slices.Equal(got, want) {
		t.Errorf("restored refs =\n%q\nwant\n%q", got, want)
	}
}

// A commit that breaks git's style rules, as old repositories have, restores
// even for a user who turned on transfer.fsckObjects.
func TestRestoreIgnoresFsckObjects(t *testing.T) {
	git := gittest.Runner(t)
	repo := mirrorOf(t, git)
	tree := gittest.Git(t, git, repo, "rev-parse", "HEAD^{tree}")
	odd := filepath.Join(t.TempDir(), "commit")
	commit := "tree " + tree + "\nauthor no email here 1700000000 +0000\ncommitter t <t@t> 1700000000 +0000\n\nodd\n"
	if err := os.WriteFile(odd, []byte(commit), 0o644); err != nil {
		t.Fatal(err)
	}
	id := gittest.Git(t, git, repo, "hash-object", "-t", "commit", "-w", "--literally", odd)
	gittest.Git(t, git, repo, "update-ref", "refs/heads/odd", id)
	path := filepath.Join(t.TempDir(), "snapshot.bundle")
	if err := Create(t.Context(), git, repo, path); err != nil {
		t.Fatal(err)
	}

	// git before 2.32 reads HOME's config, not GIT_CONFIG_GLOBAL's.
	home := t.TempDir()
	global := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(global, []byte("[transfer]\n\tfsckObjects = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	strict := &gitcmd.Runner{Env: append(slices.Clone(git.Env), "HOME="+home, "GIT_CONFIG_GLOBAL="+global)}
	if _, err := strict.Run(t.Context(), "", "clone", "--quiet", "--mirror", path, filepath.Join(t.TempDir(), "plain.git")); err == nil {
		t.Skip("this git doesn't apply transfer.fsckObjects to a bundle")
	}
	if err := Restore(t.Context(), strict, path, filepath.Join(t.TempDir(), "restored.git")); err != nil {
		t.Errorf("Restore: %v", err)
	}
}

// Restore catches what Verify can't: damaged objects, which git copies into a
// bundle as they are, and missing ones.
func TestRestoreCatchesDamage(t *testing.T) {
	git := gittest.Runner(t)
	dir := t.TempDir()

	damaged := mirrorOf(t, git)
	gittest.Git(t, git, damaged, "repack", "-a", "-d", "-q") // a local clone has loose objects
	if err := gittest.DamageObject(t.Context(), git, damaged, "HEAD:README"); err != nil {
		t.Fatal(err)
	}
	damagedBundle := filepath.Join(dir, "damaged.bundle")
	if err := Create(t.Context(), git, damaged, damagedBundle); err != nil {
		t.Fatal(err)
	}

	// A bundle of main, with a ref to a commit it lacks, as a damaged
	// reachability bitmap could give. Outside branches and tags.
	repo := mirrorOf(t, git)
	incomplete := filepath.Join(dir, "incomplete.bundle")
	gittest.Git(t, git, repo, "bundle", "create", "--quiet", incomplete, "main")
	data, err := os.ReadFile(incomplete)
	if err != nil {
		t.Fatal(err)
	}
	feature := gittest.Git(t, git, repo, "rev-parse", "feature")
	data = bytes.Replace(data, []byte("\n\n"), []byte("\n"+feature+" refs/pull/1/head\n\n"), 1)
	if err := os.WriteFile(incomplete, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Restore runs with no git config, as for a user who never set
	// init.defaultBranch: git would add a hint about it to the error. git
	// before 2.32 reads HOME's config, not GIT_CONFIG_GLOBAL's.
	home := t.TempDir()
	global := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	plain := &gitcmd.Runner{Env: append(slices.Clone(git.Env), "HOME="+home, "GIT_CONFIG_GLOBAL="+global)}

	for name, b := range map[string]struct{ repo, path string }{"damaged": {damaged, damagedBundle}, "incomplete": {repo, incomplete}} {
		// Logged rather than checked: a later git may do better.
		t.Logf("%s: Verify: %v", name, Verify(t.Context(), git, b.repo, b.path))
		_, bareErr := git.Run(t.Context(), "", "clone", "--quiet", "--bare", b.path, filepath.Join(t.TempDir(), "bare.git"))
		t.Logf("%s: a clone without --mirror: %v", name, bareErr)

		err := Restore(t.Context(), plain, b.path, filepath.Join(t.TempDir(), "restored.git"))
		if err == nil {
			t.Errorf("%s: Restore succeeded", name)
		} else if strings.Contains(err.Error(), "hint:") {
			t.Errorf("%s: Restore's error has a hint: %v", name, err)
		}
	}
}
