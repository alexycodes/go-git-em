package bundle

import (
	"os"
	"path/filepath"
	"slices"
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
