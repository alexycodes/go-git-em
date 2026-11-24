// Package backup runs one backup of one repository: it updates the mirror,
// writes a snapshot if anything changed, and applies retention. It also
// checks the backups an output already holds (Job.Check).
package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alexycodes/go-git-em/internal/bundle"
	"github.com/alexycodes/go-git-em/internal/config"
	"github.com/alexycodes/go-git-em/internal/gitcmd"
	"github.com/alexycodes/go-git-em/internal/mirror"
)

// Layout of an output directory.
const (
	MirrorDir      = "mirror.git"
	SnapshotsDir   = "snapshots"
	LockFile       = ".lock"
	TestRestoreDir = "test-restore.tmp" // a snapshot's test restore, deleted once checked
)

// ErrLocked means another process is backing up or checking the same output.
var ErrLocked = errors.New("another go-git-em is using this output")

// errNoLocks means the output's file system can't lock files.
var errNoLocks = errors.New("files can't be locked here")

// lockOutput is lock, which tests replace to see what happens without locks.
var lockOutput = lock

// staleLock is how old a lock file in the mirror must be for a run to take it
// for a killed git's. A running git holds its locks for moments, but one that
// outlived a kill -9 of go-git-em on Unix may still be at work when the next
// run starts. On Windows, git dies with go-git-em (gitcmd's job object).
var staleLock = func() time.Duration {
	if runtime.GOOS == "windows" {
		return 0
	}
	return time.Minute
}()

// Status says what a successful run did.
type Status string

const (
	Created   Status = "created"   // a new snapshot
	Unchanged Status = "unchanged" // the refs match the newest snapshot, which is intact
	Empty     Status = "empty"     // upstream has no refs, so there's nothing to snapshot
)

// Result is what a run did. A run that fails after writing its snapshot still
// reports it.
type Result struct {
	Status   Status
	Snapshot string   // the new snapshot's file name, if Created
	Pruned   []string // the snapshots retention deleted
}

// Job backs up one repository, or checks its backups.
type Job struct {
	Repo config.Repository
	Git  *gitcmd.Runner   // Repo.SSHKey replaces its SSHKey
	Now  func() time.Time // names snapshots; nil means time.Now
	Log  *slog.Logger     // for warnings, and Check's findings; nil discards them
}

// Run runs one backup within the repository's timeout. A run that fails
// before its snapshot is complete deletes no snapshot, so the older ones keep
// what upstream may have just lost.
func (j *Job) Run(ctx context.Context) (Result, error) {
	if j.Repo.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, j.Repo.Timeout)
		defer cancel()
	}
	git := *j.Git
	git.SSHKey = j.Repo.SSHKey
	log := j.logger()

	dir := filepath.Join(j.Repo.Output, SnapshotsDir)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return Result{}, err
	}
	// Another process on this output would remove this run's temporary files,
	// and race it on the mirror and the snapshots.
	unlock, locked, err := j.lock(ctx, log)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	names, temps, err := scan(dir)
	if err != nil {
		return Result{}, err
	}
	// Leftovers take space, and one could block a retry under the same name.
	if err := errors.Join(removeAll(dir, temps), os.RemoveAll(filepath.Join(j.Repo.Output, TestRestoreDir))); err != nil {
		log.WarnContext(ctx, "can't remove a temporary file", "err", err)
	}

	m := &mirror.Mirror{Path: filepath.Join(j.Repo.Output, MirrorDir), Git: &git}
	// A git killed in its work, by the wait delay or as Git for Windows is by
	// a stop, leaves lock files that make every later run fail. Only the
	// output's lock rules out another go-git-em's git.
	if locked {
		removed, err := m.RemoveStaleLocks(time.Now().Add(-staleLock))
		if len(removed) > 0 {
			// A fetch killed while it updates many refs leaves a lock for each.
			log.WarnContext(ctx, "removed lock files that a killed git left in the mirror", "count", len(removed), "files", strings.Join(removed[:min(len(removed), 5)], " "))
		}
		if err != nil {
			log.WarnContext(ctx, "can't remove lock files that git left in the mirror", "err", err)
		}
	}
	cloned, oldURL, err := m.Sync(ctx, j.Repo.Input)
	if oldURL != "" { // even if the fetch then failed: the mirror now has the new URL
		log.WarnContext(ctx, "the input changed: the mirror now fetches from it", "was", config.Redact(oldURL))
	}
	if err != nil {
		return Result{}, fmt.Errorf("update the mirror: %w", err)
	}
	// A fresh clone already has upstream's default branch. Otherwise, HEAD only
	// has to catch up eventually: the next run can do it.
	if !cloned {
		if err := m.FollowHead(ctx); err != nil {
			log.WarnContext(ctx, "can't read upstream's default branch", "err", err)
		}
	}
	refs, err := m.Fingerprint(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read the mirror's refs: %w", err)
	}
	if len(refs) == 0 {
		// No retention either: when upstream has lost everything, the
		// snapshots are all that's left of it.
		log.WarnContext(ctx, "upstream has no refs: no snapshot")
		return Result{Status: Empty}, nil
	}

	var res Result
	same := false
	if len(names) > 0 {
		if same, err = matches(ctx, &git, log, m.Path, filepath.Join(dir, names[len(names)-1]), refs); err != nil {
			return Result{}, fmt.Errorf("check the newest snapshot: %w", err)
		}
	}
	if same {
		res.Status = Unchanged
	} else {
		name, err := j.snapshot(ctx, &git, log, m, dir, names, refs)
		if name != "" {
			res = Result{Status: Created, Snapshot: name}
			names = append(names, name)
		}
		if err != nil {
			return res, err
		}
		switch lfs, err := m.UsesLFS(ctx); {
		case err != nil:
			log.WarnContext(ctx, "can't tell whether the repository uses Git LFS", "err", err)
		case lfs:
			log.WarnContext(ctx, "the repository uses Git LFS: the snapshot holds only the LFS pointer files, not their content")
		}
	}

	// Unchanged runs apply retention too, so that a lower retention takes
	// effect without waiting for an upstream change.
	var pruneErr error
	res.Pruned, pruneErr = prune(dir, names, j.Repo.Retention)
	// The snapshot is safe by now, so a failed gc is only worth a warning.
	if err := m.Housekeep(ctx); err != nil {
		log.WarnContext(ctx, "git gc failed", "err", err)
	}
	if pruneErr != nil {
		return res, fmt.Errorf("retention: %w", pruneErr)
	}
	return res, nil
}

// logger returns Log, or a logger that discards.
func (j *Job) logger() *slog.Logger {
	if j.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return j.Log
}

// lock locks the output, which must exist, until unlock. locked is false
// where files can't be locked, which lock warns about, and a run then goes on
// unlocked.
func (j *Job) lock(ctx context.Context, log *slog.Logger) (unlock func(), locked bool, err error) {
	switch unlock, err := lockOutput(filepath.Join(j.Repo.Output, LockFile)); {
	case err == nil:
		return unlock, CanLock, nil
	case errors.Is(err, errNoLocks):
		log.WarnContext(ctx, "can't lock the output: two go-git-em processes could use it at once", "err", err)
		return func() {}, false, nil
	default:
		return nil, false, fmt.Errorf("lock the output: %w", err)
	}
}

// matches reports whether the snapshot at path holds exactly refs, intact. A
// snapshot whose refs can't be read, or that is damaged, doesn't, so a new one
// is taken.
func matches(ctx context.Context, git *gitcmd.Runner, log *slog.Logger, repo, path string, refs []string) (bool, error) {
	heads, err := bundle.Heads(ctx, git, repo, path)
	if ctx.Err() != nil {
		return false, ctx.Err() // not the snapshot's fault
	}
	if err != nil {
		log.WarnContext(ctx, "can't read the newest snapshot's refs: taking a new one", "snapshot", filepath.Base(path), "err", err)
		return false, nil
	}
	if !slices.Equal(heads, refs) {
		return false, nil
	}
	// Heads reads only the header. Damage since the snapshot was written
	// would go unnoticed for as long as upstream doesn't change.
	err = bundle.CheckFile(ctx, path)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		log.WarnContext(ctx, "the newest snapshot is damaged: taking a new one", "snapshot", filepath.Base(path), "err", err)
		return false, nil
	}
	return true, nil
}

// snapshot writes a snapshot of the mirror m into dir, which holds the
// snapshots names, and returns its file name. It writes to a temporary file
// first, so that only a complete snapshot that passed its checks gets a
// snapshot's name. Once the snapshot has its name, snapshot returns it, even
// with an error.
func (j *Job) snapshot(ctx context.Context, git *gitcmd.Runner, log *slog.Logger, m *mirror.Mirror, dir string, names, refs []string) (string, error) {
	now := j.Now
	if now == nil {
		now = time.Now
	}
	name := snapshotName(now())
	// Retention keeps the latest names, so a new snapshot must sort last even
	// if the clock went back, or a snapshot was named in the future.
	if len(names) > 0 && name <= names[len(names)-1] {
		newest := names[len(names)-1]
		t, err := time.Parse(nameLayout, strings.TrimSuffix(newest, ".bundle"))
		if err != nil {
			return "", err
		}
		name = snapshotName(t.Add(time.Millisecond))
		if !isSnapshot(name) {
			return "", fmt.Errorf("can't name a snapshot after %s", newest)
		}
		log.WarnContext(ctx, "the clock is behind the newest snapshot: naming this one just after it", "newest", newest, "snapshot", name)
	}
	path := filepath.Join(dir, name)
	// os.Rename replaces an existing file. Only another process using the
	// same output could race this check, where the output's lock can't keep it
	// out.
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("snapshot %s already exists", name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	tmp := filepath.Join(dir, "."+name+".tmp")
	err := writeSnapshot(ctx, git, m.Path, tmp, refs)
	if err == nil && j.Repo.TestRestore {
		err = testRestore(ctx, git, log, tmp, filepath.Join(j.Repo.Output, TestRestoreDir))
	}
	if err != nil {
		err = blameMirror(ctx, m, err)
	} else {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp) // the next run would remove it anyway
		return "", err
	}
	// Retention deletes older snapshots next: the new one must survive a crash.
	if err := syncDir(dir); err != nil && !unsupported(err) {
		return name, fmt.Errorf("flush %s: %w", dir, err)
	}
	return name, nil
}

func writeSnapshot(ctx context.Context, git *gitcmd.Runner, repo, path string, refs []string) error {
	if err := bundle.Create(ctx, git, repo, path); err != nil {
		return fmt.Errorf("create the snapshot: %w", err)
	}
	if err := syncFile(path); err != nil {
		return err
	}
	if err := bundle.Verify(ctx, git, repo, path); err != nil {
		return fmt.Errorf("verify the snapshot: %w", err)
	}
	heads, err := bundle.Heads(ctx, git, repo, path)
	if err != nil {
		return fmt.Errorf("read the snapshot's refs: %w", err)
	}
	if !slices.Equal(heads, refs) {
		return errors.New("the snapshot's refs differ from the mirror's")
	}
	return nil
}

// testRestore restores the snapshot at path into dir, then deletes dir.
// writeSnapshot's checks read the pack's bytes, not its objects, and git
// copies the mirror's objects into a bundle without checking them: damage in
// the mirror would pass.
func testRestore(ctx context.Context, git *gitcmd.Runner, log *slog.Logger, path, dir string) error {
	err := bundle.Restore(ctx, git, path, dir)
	if rmErr := os.RemoveAll(dir); rmErr != nil { // else the next run removes it
		log.WarnContext(ctx, "can't remove the test restore", "err", rmErr)
	}
	if err != nil {
		return fmt.Errorf("test restore: %w", err)
	}
	return nil
}

// blameMirror checks the mirror's objects after git failed to write or check
// a snapshot with err, and adds to err that the mirror is damaged if they
// are. Damage there spoils every new snapshot until the mirror is cloned
// again, and git's errors don't say where the damage is.
func blameMirror(ctx context.Context, m *mirror.Mirror, err error) error {
	if ctx.Err() != nil || gitcmd.ExitCode(err) <= 0 {
		return err // the mirror's data can't have caused it
	}
	// Other failures, such as a pack directory that can't be read, are no
	// reason to delete the mirror.
	if verifyErr := m.Verify(ctx); mirrorDamaged(verifyErr) && ctx.Err() == nil {
		return fmt.Errorf("%w; the mirror is damaged: delete %s, and the next run clones it again: %w", err, m.Path, verifyErr)
	}
	return err
}

// syncFile flushes a file to disk. On Windows, that needs the file open for
// writing.
func syncFile(path string) error {
	if runtime.GOOS == "windows" {
		return syncPath(path, os.O_RDWR)
	}
	return syncPath(path, os.O_RDONLY)
}

// syncDir flushes a directory's entries to disk, such as a file just renamed
// into it. Windows can't: a directory can't be opened for writing there.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return syncPath(dir, os.O_RDONLY)
}

func syncPath(path string, flag int) error {
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// unsupported reports whether err says the file system can't flush a
// directory or lock a file, as some network and FUSE file systems can't.
func unsupported(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.EINVAL)
}

// scan returns the snapshots in dir, oldest first, and the temporary files
// that interrupted runs left there.
func scan(dir string) (snapshots, temps []string, err error) {
	entries, err := os.ReadDir(dir) // sorted by name, which is time order
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		switch {
		case isSnapshot(e.Name()):
			snapshots = append(snapshots, e.Name())
		case isTemp(e.Name()):
			temps = append(temps, e.Name())
		}
	}
	return snapshots, temps, nil
}

// removeAll removes the named files from dir.
func removeAll(dir string, names []string) error {
	var errs []error
	for _, name := range names {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// prune deletes the snapshots beyond the keep latest, and returns those it
// deleted.
func prune(dir string, snapshots []string, keep int) ([]string, error) {
	var pruned []string
	var errs []error
	for _, name := range toDelete(snapshots, keep) {
		switch err := os.Remove(filepath.Join(dir, name)); {
		case err == nil:
			pruned = append(pruned, name)
		case !errors.Is(err, fs.ErrNotExist): // already gone isn't this run's doing
			errs = append(errs, err)
		}
	}
	return pruned, errors.Join(errs...)
}
