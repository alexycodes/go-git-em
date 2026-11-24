package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexycodes/go-git-em/internal/gittest"
)

func (e *env) check(t *testing.T, full bool, want CheckResult) {
	t.Helper()
	got, err := e.job.Check(t.Context(), full)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Check(full %v) = %+v, want %+v", full, got, want)
	}
}

func TestCheckIntact(t *testing.T) {
	e := newEnv(t)
	e.run(t, Created)
	e.change(t)
	e.run(t, Created)
	before := e.dirNames(t)
	// An interrupted test restore, which the full check's would clash with.
	if err := os.MkdirAll(filepath.Join(e.job.Repo.Output, TestRestoreDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, full := range []bool{false, true} {
		e.check(t, full, CheckResult{Snapshots: 2, Mirror: MirrorIntact})
	}
	if e.log.Len() != 0 {
		t.Errorf("unexpected logs:\n%s", e.log)
	}
	if got := e.dirNames(t); !slices.Equal(got, before) {
		t.Errorf("snapshots directory = %q, want %q", got, before)
	}
	if _, err := os.Stat(filepath.Join(e.job.Repo.Output, TestRestoreDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("test restore: %v, want it gone", err)
	}
}

// An output that doesn't exist fails the check: it was never backed up, or
// isn't mounted. The check creates nothing.
func TestCheckNothingBackedUp(t *testing.T) {
	e := newEnv(t)
	res, err := e.job.Check(t.Context(), true)
	if want := (CheckResult{Mirror: MirrorMissing}); res != want || !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "nothing to check: ") {
		t.Errorf("Check = %+v, %v; want %+v and nothing to check", res, err, want)
	}
	if _, err := os.Stat(e.job.Repo.Output); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("output: %v, want none", err)
	}
}

// An output that can't be written, such as a read-only mount, can't be
// locked, but can be checked.
func TestCheckReadOnly(t *testing.T) {
	e := newEnv(t)
	e.run(t, Created)
	lockOutput = func(path string) (func(), error) {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
	}
	t.Cleanup(func() { lockOutput = lock })
	e.check(t, false, CheckResult{Snapshots: 1, Mirror: MirrorIntact})
	if want := `level=WARN msg="can't lock the output, which can't be written: checking it unlocked"`; !strings.Contains(e.log.String(), want) {
		t.Errorf("logs lack %q:\n%s", want, e.log)
	}
}

// A loose object, as a small fetch leaves, damaged in the mirror.
func TestCheckDamagedLooseObject(t *testing.T) {
	e := newEnv(t)
	e.run(t, Created)
	gittest.Commit(t, e.git, e.src, "loose", map[string]string{"loose.txt": "a loose blob\n"})
	e.run(t, Created)
	mirror := filepath.Join(e.job.Repo.Output, MirrorDir)
	id := gittest.Git(t, e.git, mirror, "rev-parse", "HEAD:loose.txt")
	loose := filepath.Join(mirror, "objects", id[:2], id[2:])
	if err := os.Chmod(loose, 0o644); err != nil { // git writes objects read-only
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, []byte("not zlib"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.check(t, false, CheckResult{Snapshots: 2, Mirror: MirrorDamaged})
	if want := `msg="the mirror is damaged: delete it, and the next backup clones it again" mirror=` + mirror + ` err="damaged: loose object ` + id; !strings.Contains(e.log.String(), want) {
		t.Errorf("logs lack %q:\n%s", want, e.log)
	}
}

func TestCheckDamagedSnapshot(t *testing.T) {
	e := newEnv(t)
	name := e.run(t, Created).Snapshot
	e.change(t)
	e.run(t, Created)
	path := filepath.Join(e.job.Repo.Output, SnapshotsDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(data) - 30; i < len(data)-26; i++ { // in the pack, before its checksum
		data[i] ^= 0xff
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	e.check(t, false, CheckResult{Snapshots: 2, Damaged: 1, Mirror: MirrorIntact})
	if want := `level=ERROR msg="a snapshot is damaged" snapshot=` + name + ` err=`; !strings.Contains(e.log.String(), want) {
		t.Errorf("logs lack %q:\n%s", want, e.log)
	}
}

// A blob damaged in the mirror reaches a snapshot taken without a test
// restore. Its checksum is valid, so only a full check finds it.
func TestCheckDamagedMirror(t *testing.T) {
	e := newEnv(t)
	e.run(t, Created)
	mirror := filepath.Join(e.job.Repo.Output, MirrorDir)
	if err := gittest.DamageObject(t.Context(), e.git, mirror, "HEAD:README"); err != nil {
		t.Fatal(err)
	}
	e.change(t)
	e.job.Repo.TestRestore = false
	name := e.run(t, Created).Snapshot

	e.check(t, false, CheckResult{Snapshots: 2, Mirror: MirrorDamaged})
	if want := `level=ERROR msg="the mirror is damaged: delete it, and the next backup clones it again" mirror=` + mirror + ` err=`; !strings.Contains(e.log.String(), want) {
		t.Errorf("logs lack %q:\n%s", want, e.log)
	}
	e.log.Reset()
	e.check(t, true, CheckResult{Snapshots: 2, Damaged: 1, Mirror: MirrorDamaged})
	if want := `level=ERROR msg="a snapshot is damaged" snapshot=` + name + ` err="test restore: `; !strings.Contains(e.log.String(), want) {
		t.Errorf("logs lack %q:\n%s", want, e.log)
	}
	if strings.Count(e.log.String(), "a snapshot is damaged") != 1 {
		t.Errorf("the snapshot taken before the damage isn't intact:\n%s", e.log)
	}
}

// A test restore that fails for a cause outside the snapshot, such as a full
// disk, leaves it unchecked, not damaged.
func TestCheckRestoreOutsideCause(t *testing.T) {
	e := newEnv(t)
	name := e.run(t, Created).Snapshot
	// Only the test restore starts with -c core.fsync=none.
	e.job.Git = gittest.FakeGit(t, e.git.Env, `if [ "$2" = core.fsync=none ]; then echo "fatal: write error: No space left on device" >&2; exit 128; fi
exec "$GIT" "$@"`)
	res, err := e.job.Check(t.Context(), true)
	if want := (CheckResult{Mirror: MirrorIntact}); res != want {
		t.Errorf("Check = %+v, want %+v", res, want)
	}
	if err == nil || !strings.Contains(err.Error(), "snapshot "+name+": test restore: ") {
		t.Errorf("error = %v, want snapshot %s unchecked", err, name)
	}
	if strings.Contains(e.log.String(), "damaged") {
		t.Errorf("logs blame damage:\n%s", e.log)
	}
}

// verify-pack failing for a cause outside the mirror's data leaves the mirror
// unchecked: no one should delete it for that.
func TestCheckMirrorOutsideCause(t *testing.T) {
	e := newEnv(t)
	e.run(t, Created)
	// git gets --git-dir=<mirror> verify-pack <idx>.
	e.job.Git = gittest.FakeGit(t, e.git.Env, `if [ "$2" = verify-pack ]; then echo "fatal: cannot open $3: Permission denied" >&2; exit 128; fi
exec "$GIT" "$@"`)
	res, err := e.job.Check(t.Context(), false)
	if want := (CheckResult{Snapshots: 1, Mirror: MirrorUnchecked}); res != want || err == nil || !strings.Contains(err.Error(), "mirror: ") {
		t.Errorf("Check = %+v, %v; want %+v and the mirror unchecked", res, err, want)
	}
	if strings.Contains(e.log.String(), "damaged") {
		t.Errorf("logs blame damage:\n%s", e.log)
	}
}

func TestOutsideCause(t *testing.T) {
	for msg, want := range map[string]bool{
		"fatal: write error: No space left on device":      true,
		"error: unable to write: Disk quota exceeded":      true, // glibc
		"error: unable to write: Disc quota exceeded":      true, // macOS
		"error: unable to write: Quota exceeded":           true, // musl
		"fatal: unable to create: Permission denied":       true,
		"fatal: unable to create: Read-only file system":   true,
		"fatal: Out of memory, malloc failed":              true,
		"error: index-pack died of signal 9":               true,
		"fatal: pack has bad object at offset 183":         false,
		"fatal: remote did not send all necessary objects": false,
	} {
		if got := outsideCause(errors.New(msg)); got != want {
			t.Errorf("outsideCause(%q) = %v, want %v", msg, got, want)
		}
	}
}

// A cancelled check stops, and blames nothing.
func TestCheckCancelled(t *testing.T) {
	e := newEnv(t)
	e.run(t, Created)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.job.Check(ctx, true); !errors.Is(err, context.Canceled) {
		t.Errorf("Check = %v, want context.Canceled", err)
	}
	if e.log.Len() != 0 {
		t.Errorf("unexpected logs:\n%s", e.log)
	}
}

// What can't be checked is an error, not damage, and doesn't stop the rest.
func TestCheckUnchecked(t *testing.T) {
	e := newEnv(t)
	e.run(t, Created)
	e.change(t)
	e.run(t, Created)

	// Each test restore outlasts the timeout, which applies to each one.
	// Only the test restore starts with -c core.fsync=none.
	e.job.Git = gittest.FakeGit(t, e.git.Env, `if [ "$2" = core.fsync=none ]; then sleep 10; fi
exec "$GIT" "$@"`)
	e.job.Repo.Timeout = 500 * time.Millisecond
	// The pack directory is a file: verify-pack can't run, which isn't damage.
	pack := filepath.Join(e.job.Repo.Output, MirrorDir, "objects", "pack")
	if err := os.Rename(pack, pack+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pack, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res, err := e.job.Check(t.Context(), true)
	if want := (CheckResult{Mirror: MirrorUnchecked}); res != want {
		t.Errorf("Check = %+v, want %+v", res, want)
	}
	for _, name := range e.snapshots(t) {
		if err == nil || !strings.Contains(err.Error(), "snapshot "+name+": test restore: ") {
			t.Errorf("error = %v, want snapshot %s unchecked", err, name)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "mirror: ") {
		t.Errorf("error = %v, want the timeouts and the mirror", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Check took %v", d)
	}
	if strings.Contains(e.log.String(), "damaged") {
		t.Errorf("logs blame damage:\n%s", e.log)
	}
}
