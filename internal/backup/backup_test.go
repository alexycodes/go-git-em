package backup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexycodes/go-git-em/internal/config"
	"github.com/alexycodes/go-git-em/internal/gitcmd"
	"github.com/alexycodes/go-git-em/internal/gittest"
)

// env is a fixture repository, and a Job that backs it up.
type env struct {
	git *gitcmd.Runner
	src string
	job *Job
	log *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	git := gittest.Runner(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	gittest.Fixture(t, git, src)
	var log bytes.Buffer
	now := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	return &env{git: git, src: src, log: &log, job: &Job{
		Repo: config.Repository{Input: src, Output: filepath.Join(dir, "out"), Timeout: time.Minute},
		Git:  git,
		Now:  func() time.Time { now = now.Add(time.Second); return now },
		Log:  slog.New(slog.NewTextHandler(&log, nil)),
	}}
}

func (e *env) run(t *testing.T, want Status) Result {
	t.Helper()
	res, err := e.job.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != want {
		t.Fatalf("status = %s, want %s", res.Status, want)
	}
	return res
}

func (e *env) change(t *testing.T) {
	t.Helper()
	gittest.Commit(t, e.git, e.src, "change", nil)
}

func (e *env) snapshots(t *testing.T) []string {
	t.Helper()
	names, _, err := scan(filepath.Join(e.job.Repo.Output, SnapshotsDir))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func (e *env) dirNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.job.Repo.Output, SnapshotsDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestRunCreatedUnchanged(t *testing.T) {
	e := newEnv(t)
	res := e.run(t, Created)
	if res.Snapshot != "20261101T230001.000Z.bundle" || len(res.Pruned) != 0 {
		t.Errorf("first run: %+v", res)
	}
	if res := e.run(t, Unchanged); res.Snapshot != "" {
		t.Errorf("unchanged run: %+v", res)
	}
	e.change(t)
	if res := e.run(t, Created); res.Snapshot != "20261101T230002.000Z.bundle" {
		t.Errorf("after a change: %+v", res)
	}
	if got := e.snapshots(t); len(got) != 2 {
		t.Errorf("snapshots = %q, want 2", got)
	}
}

// Every snapshot restores with plain git, passes fsck --strict, and holds
// the source's refs as they were, through force-pushes and deleted branches.
func TestRunRoundTrip(t *testing.T) {
	e := newEnv(t)
	var states [][]string
	snapshot := func() {
		e.run(t, Created)
		states = append(states, gittest.Refs(t, e.git, e.src))
	}
	snapshot()
	gittest.Git(t, e.git, e.src, "switch", "--quiet", "feature")
	gittest.Git(t, e.git, e.src, "reset", "--quiet", "--hard", "HEAD~1") // force-push
	gittest.Commit(t, e.git, e.src, "rewritten", map[string]string{"other.bin": "\x00\xff"})
	gittest.Git(t, e.git, e.src, "switch", "--quiet", "main")
	snapshot()
	gittest.Git(t, e.git, e.src, "branch", "--delete", "--force", "same-tip")
	gittest.Git(t, e.git, e.src, "tag", "--delete", "v1")
	snapshot()

	names := e.snapshots(t)
	if len(names) != len(states) {
		t.Fatalf("snapshots = %q, want %d", names, len(states))
	}
	for i, name := range names {
		bundle := filepath.Join(e.job.Repo.Output, SnapshotsDir, name)
		restored := filepath.Join(t.TempDir(), "restored.git")
		gittest.Git(t, e.git, "", "clone", "--quiet", "--mirror", bundle, restored)
		gittest.Git(t, e.git, restored, "fsck", "--strict", "--no-progress")
		if got := gittest.Refs(t, e.git, restored); !slices.Equal(got, states[i]) {
			t.Errorf("snapshot %d restores refs\n%q\nwant\n%q", i+1, got, states[i])
		}
	}

	// A plain clone checks out the default branch, binary file included.
	work := filepath.Join(t.TempDir(), "work")
	gittest.Git(t, e.git, "", "clone", "--quiet", filepath.Join(e.job.Repo.Output, SnapshotsDir, names[0]), work)
	data, err := os.ReadFile(filepath.Join(work, "data.bin"))
	if err != nil || !bytes.Equal(data, []byte("\x00\x01\x02\xff\xfe binary \x00")) {
		t.Errorf("data.bin = %q, %v", data, err)
	}

	// After upstream renames its default branch, a snapshot still checks it out.
	gittest.Git(t, e.git, e.src, "branch", "--move", "main", "trunk")
	e.change(t)
	name := e.run(t, Created).Snapshot
	work = filepath.Join(t.TempDir(), "work")
	gittest.Git(t, e.git, "", "clone", "--quiet", filepath.Join(e.job.Repo.Output, SnapshotsDir, name), work)
	if branch := gittest.Git(t, e.git, work, "branch", "--show-current"); branch != "trunk" {
		t.Errorf("the clone checked out %q, want trunk", branch)
	}
}

func TestRunEmptyUpstream(t *testing.T) {
	e := newEnv(t)
	empty := filepath.Join(t.TempDir(), "empty.git")
	gittest.Git(t, e.git, "", "init", "--quiet", "--bare", empty)
	e.job.Repo.Input = empty

	e.run(t, Empty)
	if got := e.snapshots(t); len(got) != 0 {
		t.Errorf("snapshots = %q, want none", got)
	}
	if !strings.Contains(e.log.String(), "upstream has no refs") {
		t.Errorf("no warning about the empty upstream:\n%s", e.log)
	}
	gittest.Git(t, e.git, e.src, "push", "--quiet", empty, "main")
	e.run(t, Created)
}

func TestRunRetention(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.job.Repo.Output, SnapshotsDir)
	e.job.Repo.Retention = 2
	var created []string
	for range 4 {
		created = append(created, e.run(t, Created).Snapshot)
		e.change(t)
	}
	if got := e.snapshots(t); !slices.Equal(got, created[2:]) {
		t.Errorf("snapshots = %q, want the 2 latest %q", got, created[2:])
	}

	// Files that aren't snapshots stay, even when they look like one.
	others := []string{"notes.txt", "keep.bundle", "20261101T230000Z.bundle", ".notes.tmp"}
	for _, name := range others {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "20200101T000000.000Z.bundle"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, Created)
	if !slices.Equal(res.Pruned, created[2:3]) {
		t.Errorf("pruned %q, want %q", res.Pruned, created[2:3])
	}
	want := append(slices.Clone(others), "20200101T000000.000Z.bundle", created[3], res.Snapshot)
	slices.Sort(want)
	if got := e.dirNames(t); !slices.Equal(got, want) {
		t.Errorf("snapshots directory = %q, want %q", got, want)
	}

	// A lower retention takes effect without an upstream change...
	e.job.Repo.Retention = 1
	if res := e.run(t, Unchanged); !slices.Equal(res.Pruned, []string{created[3]}) {
		t.Errorf("unchanged run pruned %q, want %q", res.Pruned, created[3])
	}
	// ...and 0 keeps everything.
	e.job.Repo.Retention = 0
	for range 3 {
		e.change(t)
		if res := e.run(t, Created); len(res.Pruned) != 0 {
			t.Errorf("retention 0 pruned %q", res.Pruned)
		}
	}
	if got := e.snapshots(t); len(got) != 4 {
		t.Errorf("snapshots = %q, want 4", got)
	}
}

func TestRunFailureDeletesNothing(t *testing.T) {
	e := newEnv(t)
	for range 3 {
		e.run(t, Created)
		e.change(t)
	}
	before := e.dirNames(t)
	mirror := filepath.Join(e.job.Repo.Output, MirrorDir)
	refs := gittest.Refs(t, e.git, mirror)

	// The input is gone, and retention would now keep only 1 snapshot.
	if err := os.Rename(e.src, e.src+"-gone"); err != nil {
		t.Fatal(err)
	}
	e.job.Repo.Retention = 1
	if _, err := e.job.Run(t.Context()); err == nil {
		t.Fatal("Run succeeded without its input")
	}
	if got := e.dirNames(t); !slices.Equal(got, before) {
		t.Errorf("after a failed run: snapshots directory = %q, want %q", got, before)
	}
	if got := gittest.Refs(t, e.git, mirror); !slices.Equal(got, refs) {
		t.Errorf("after a failed run: mirror refs = %q, want %q", got, refs)
	}
}

// A clock that doesn't move forward, or goes back, would give the new
// snapshot the oldest name, and retention would prune it first. So it's
// named just after the newest.
func TestRunClockBehind(t *testing.T) {
	for name, delta := range map[string]time.Duration{"same time": 0, "earlier": -time.Hour} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			at := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
			e.job.Now = func() time.Time { return at }
			e.run(t, Created)

			at = at.Add(delta)
			e.job.Repo.Retention = 1
			e.change(t)
			res := e.run(t, Created)
			if res.Snapshot != "20261101T230000.001Z.bundle" {
				t.Errorf("snapshot = %s, want one just after 20261101T230000.000Z.bundle", res.Snapshot)
			}
			if got := e.dirNames(t); !slices.Equal(got, []string{res.Snapshot}) {
				t.Errorf("snapshots directory = %q, want only the new snapshot", got)
			}
			if !strings.Contains(e.log.String(), "the clock is behind the newest snapshot") {
				t.Errorf("no warning about the clock:\n%s", e.log)
			}
		})
	}
}

// When upstream makes another branch the default, without changing any
// branch, the next snapshot must restore to it.
func TestRunDefaultBranchChange(t *testing.T) {
	e := newEnv(t)
	e.run(t, Created)
	gittest.Git(t, e.git, e.src, "switch", "--quiet", "feature")
	name := e.run(t, Created).Snapshot
	work := filepath.Join(t.TempDir(), "work")
	gittest.Git(t, e.git, "", "clone", "--quiet", filepath.Join(e.job.Repo.Output, SnapshotsDir, name), work)
	if branch := gittest.Git(t, e.git, work, "branch", "--show-current"); branch != "feature" {
		t.Errorf("the clone checked out %q, want feature", branch)
	}
}

// The mirror switches to the new input before fetching, so the warning must
// come even if that first fetch fails.
func TestRunInputChangedFetchFails(t *testing.T) {
	e := newEnv(t)
	e.run(t, Created)
	e.job.Repo.Input = filepath.Join(t.TempDir(), "missing")
	if _, err := e.job.Run(t.Context()); err == nil {
		t.Fatal("Run succeeded")
	}
	if !strings.Contains(e.log.String(), "the input changed") {
		t.Errorf("no warning about the changed input:\n%s", e.log)
	}
}

func TestMatchesTimeout(t *testing.T) {
	e := newEnv(t)
	name := e.run(t, Created).Snapshot
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	path := filepath.Join(e.job.Repo.Output, SnapshotsDir, name)
	if _, err := matches(ctx, e.git, e.job.Log, filepath.Join(e.job.Repo.Output, MirrorDir), path, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if strings.Contains(e.log.String(), "can't read") {
		t.Errorf("a cancelled run blamed the snapshot:\n%s", e.log)
	}
}

// Whatever already has the new snapshot's name stays.
func TestRunNeverOverwrites(t *testing.T) {
	e := newEnv(t)
	taken := filepath.Join(e.job.Repo.Output, SnapshotsDir, "20261101T230001.000Z.bundle")
	if err := os.MkdirAll(filepath.Join(taken, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.job.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(taken, "inside")); err != nil {
		t.Errorf("what had the name changed: %v", err)
	}
	if got := e.dirNames(t); !slices.Equal(got, []string{filepath.Base(taken)}) {
		t.Errorf("snapshots directory = %q", got)
	}
}

func TestRunRemovesTemporaryFiles(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.job.Repo.Output, SnapshotsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	leftovers := []string{".20261101T220000.000Z.bundle.tmp", ".20261101T220000.000Z.bundle.tmp.lock"}
	for _, name := range append(slices.Clone(leftovers), ".notes.tmp") {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A directory with a leftover's name isn't one, and mustn't fail the run.
	if err := os.MkdirAll(filepath.Join(dir, ".20261101T210000.000Z.bundle.tmp", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, Created)
	if got, want := e.dirNames(t), []string{".20261101T210000.000Z.bundle.tmp", ".notes.tmp", res.Snapshot}; !slices.Equal(got, want) {
		t.Errorf("snapshots directory = %q, want %q", got, want)
	}
	if e.log.Len() != 0 {
		t.Errorf("unexpected warnings:\n%s", e.log)
	}
}

func TestRunUnreadableNewest(t *testing.T) {
	e := newEnv(t)
	name := e.run(t, Created).Snapshot
	if err := os.WriteFile(filepath.Join(e.job.Repo.Output, SnapshotsDir, name), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.run(t, Created)
	if !strings.Contains(e.log.String(), "can't read the newest snapshot's refs") {
		t.Errorf("no warning about the unreadable snapshot:\n%s", e.log)
	}
}

func TestRunWarnings(t *testing.T) {
	e := newEnv(t)
	e.run(t, Created)
	if e.log.Len() != 0 {
		t.Errorf("unexpected warnings:\n%s", e.log)
	}

	// The input moved.
	moved := filepath.Join(t.TempDir(), "moved.git")
	gittest.Git(t, e.git, "", "clone", "--quiet", "--bare", e.src, moved)
	e.job.Repo.Input = moved
	e.run(t, Unchanged)
	if !strings.Contains(e.log.String(), `msg="the input changed: the mirror now fetches from it" was=`+e.src) {
		t.Errorf("no warning about the changed input:\n%s", e.log)
	}

	// The repository starts using LFS.
	e.log.Reset()
	e.job.Repo.Input = e.src
	gittest.Commit(t, e.git, e.src, "lfs", map[string]string{".gitattributes": "*.bin filter=lfs diff=lfs merge=lfs -text\n"})
	e.run(t, Created)
	if !strings.Contains(e.log.String(), "the repository uses Git LFS") {
		t.Errorf("no warning about LFS:\n%s", e.log)
	}
}

func TestRunTimeout(t *testing.T) {
	e := newEnv(t)
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()
	e.job.Repo.Input = hang.URL + "/repo.git"
	e.job.Repo.Timeout = time.Second

	start := time.Now()
	_, err := e.job.Run(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the timeout", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Run took %v with a 1s timeout", d)
	}
}

// A backup needs no program but git: with only git on PATH, it still works.
func TestRunOnlyGit(t *testing.T) {
	e := newEnv(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(realGit, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	env := slices.DeleteFunc(slices.Clone(e.git.Env), func(kv string) bool { return strings.HasPrefix(kv, "PATH=") })
	e.job.Git = &gitcmd.Runner{Path: filepath.Join(bin, "git"), Env: append(env, "PATH="+bin)}
	e.run(t, Created)
	e.change(t)
	e.run(t, Created)
}

// A snapshot that doesn't verify, or that misses refs, never gets a
// snapshot's name.
func TestRunRejectsBadSnapshots(t *testing.T) {
	// git gets --git-dir=<mirror> bundle create --quiet <path> --all.
	scripts := map[string]string{
		// Damages the pack, which only CheckPack notices.
		"damaged": `"$GIT" "$@" || exit
if [ "$2 $3" = "bundle create" ]; then
	size=$(wc -c < "$5")
	printf XXXX | dd of="$5" bs=1 seek=$((size - 30)) conv=notrunc 2>/dev/null
fi`,
		// Writes a valid bundle of main only.
		"missing refs": `if [ "$2 $3" = "bundle create" ]; then
	exec "$GIT" "$1" bundle create --quiet "$5" refs/heads/main
fi
exec "$GIT" "$@"`,
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.job.Git = gittest.FakeGit(t, e.git.Env, script)
			res, err := e.job.Run(t.Context())
			if err == nil {
				t.Fatal("Run succeeded")
			}
			if res.Status != "" || res.Snapshot != "" {
				t.Errorf("a failed run reports %+v", res)
			}
			if got := e.dirNames(t); len(got) != 0 {
				t.Errorf("snapshots directory = %q, want it empty", got)
			}
		})
	}
}

// An upstream that loses every ref prunes nothing: the snapshots are all
// that's left of it.
func TestRunUpstreamEmptied(t *testing.T) {
	e := newEnv(t)
	for range 3 {
		e.run(t, Created)
		e.change(t)
	}
	before := e.dirNames(t)
	for _, ref := range gittest.Refs(t, e.git, e.src) {
		gittest.Git(t, e.git, e.src, "update-ref", "-d", strings.Fields(ref)[1])
	}
	e.job.Repo.Retention = 1
	e.run(t, Empty)
	if got := e.dirNames(t); !slices.Equal(got, before) {
		t.Errorf("snapshots directory = %q, want %q", got, before)
	}
}

func TestRunUsesSSHKey(t *testing.T) {
	e := newEnv(t)
	log := filepath.Join(t.TempDir(), "ssh-command")
	e.job.Git = gittest.FakeGit(t, e.git.Env, `echo "$GIT_SSH_COMMAND" >> '`+log+`'
exec "$GIT" "$@"`)
	e.job.Repo.SSHKey = "/keys/id_ed25519"
	e.run(t, Created)
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "-i '/keys/id_ed25519'") {
		t.Errorf("git ran with GIT_SSH_COMMAND:\n%s", data)
	}
}

// A fresh clone already has upstream's default branch, so the first run
// doesn't ask for it. Later, failing to read it doesn't fail the run: HEAD
// only has to catch up eventually.
func TestRunFollowHead(t *testing.T) {
	e := newEnv(t)
	log := filepath.Join(t.TempDir(), "args")
	e.job.Git = gittest.FakeGit(t, e.git.Env, `echo "$2" >> '`+log+`'
if [ "$2" = ls-remote ] && [ -e '`+log+`.fail' ]; then echo "ls-remote failed" >&2; exit 1; fi
exec "$GIT" "$@"`)
	e.run(t, Created)
	if data, _ := os.ReadFile(log); strings.Contains(string(data), "ls-remote") {
		t.Error("the first run asked for upstream's default branch")
	}

	if err := os.WriteFile(log+".fail", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.run(t, Unchanged)
	if !strings.Contains(e.log.String(), "can't read upstream's default branch") {
		t.Errorf("no warning:\n%s", e.log)
	}
}

func TestRunOddNames(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.job.Repo.Output, SnapshotsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The shape of a snapshot, but not a time: not one, so not the newest.
	odd := "20261340T999999.000Z.bundle"
	if err := os.WriteFile(filepath.Join(dir, odd), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.job.Repo.Retention = 1
	first := e.run(t, Created).Snapshot
	e.change(t)
	second := e.run(t, Created).Snapshot
	if got := e.dirNames(t); !slices.Equal(got, []string{second, odd}) {
		t.Errorf("snapshots directory = %q, want %q and the odd file", got, second)
	}
	if first >= second {
		t.Errorf("snapshots %s then %s", first, second)
	}

	// After the last representable time, there's no name left.
	last := "99991231T235959.999Z.bundle"
	if err := os.WriteFile(filepath.Join(dir, last), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.change(t)
	if _, err := e.job.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "can't name a snapshot after "+last) {
		t.Errorf("error = %v", err)
	}
}

// A leftover git lock under the name a retry will use mustn't block it.
func TestRunLeftoverLock(t *testing.T) {
	e := newEnv(t)
	at := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	e.job.Now = func() time.Time { return at } // the clock is stuck: retries reuse a name
	e.run(t, Created)
	lock := filepath.Join(e.job.Repo.Output, SnapshotsDir, ".20261101T230000.001Z.bundle.tmp.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.change(t)
	if res := e.run(t, Created); res.Snapshot != "20261101T230000.001Z.bundle" {
		t.Errorf("snapshot = %s", res.Snapshot)
	}
}
