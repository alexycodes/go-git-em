package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexycodes/go-git-em/internal/bundle"
	"github.com/alexycodes/go-git-em/internal/gitcmd"
	"github.com/alexycodes/go-git-em/internal/mirror"
)

// MirrorStatus says what a check found in the mirror.
type MirrorStatus string

const (
	MirrorIntact    MirrorStatus = "intact"
	MirrorDamaged   MirrorStatus = "damaged"
	MirrorMissing   MirrorStatus = "missing"   // not cloned yet, or deleted
	MirrorUnchecked MirrorStatus = "unchecked" // git verify-pack couldn't run
)

// CheckResult is what a check found.
type CheckResult struct {
	Snapshots int // snapshots checked
	Damaged   int // of those, the damaged ones
	Mirror    MirrorStatus
}

// Check checks the output's backups: each snapshot's checksum, and with full
// a test restore of each, then the mirror's objects (mirror.Verify). The
// repository's timeout applies to each of these on its own. Check holds the
// output's lock, so that no backup changes the snapshots meanwhile, unless
// the output is read-only, and writes nothing but LockFile and
// TestRestoreDir. It logs each damaged snapshot, and a damaged mirror, as an
// error. The error is what stopped the check, or what couldn't be checked.
func (j *Job) Check(ctx context.Context, full bool) (CheckResult, error) {
	git := *j.Git
	log := j.logger()
	// Every backup run creates the output: without it, nothing was ever
	// backed up here, or a volume isn't mounted. A check creates nothing.
	if _, err := os.Stat(j.Repo.Output); err != nil {
		var res CheckResult
		if errors.Is(err, fs.ErrNotExist) {
			res.Mirror = MirrorMissing
		}
		return res, fmt.Errorf("nothing to check: %w", err)
	}
	switch unlock, _, err := j.lock(ctx, log); {
	case err == nil:
		defer unlock()
	case readOnly(err):
		// Such as a read-only mount of the backups, which a backup run
		// elsewhere could still change meanwhile.
		log.WarnContext(ctx, "can't lock the output, which can't be written: checking it unlocked", "err", err)
	default:
		return CheckResult{}, err
	}

	dir := filepath.Join(j.Repo.Output, SnapshotsDir)
	names, _, err := scan(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return CheckResult{}, err
	}
	restoreDir := filepath.Join(j.Repo.Output, TestRestoreDir)
	var res CheckResult
	var unchecked []error
	for _, name := range names {
		var damaged bool
		err := j.timed(ctx, func(ctx context.Context) (err error) {
			damaged, err = checkSnapshot(ctx, &git, log, filepath.Join(dir, name), restoreDir, full)
			return err
		})
		switch {
		case damaged: // even if a signal came since
			log.ErrorContext(ctx, "a snapshot is damaged", "snapshot", name, "err", err)
			res.Damaged++
		case ctx.Err() != nil:
			return res, errors.Join(append(unchecked, ctx.Err())...)
		case err != nil:
			unchecked = append(unchecked, fmt.Errorf("snapshot %s: %w", name, err))
			continue
		}
		res.Snapshots++
	}

	m := &mirror.Mirror{Path: filepath.Join(j.Repo.Output, MirrorDir), Git: &git}
	if _, err := os.Stat(m.Path); errors.Is(err, fs.ErrNotExist) {
		res.Mirror = MirrorMissing
		return res, errors.Join(unchecked...)
	}
	err = j.timed(ctx, m.Verify)
	switch {
	case err == nil:
		res.Mirror = MirrorIntact
	case ctx.Err() != nil:
		return res, errors.Join(append(unchecked, ctx.Err())...)
	case mirrorDamaged(err):
		log.ErrorContext(ctx, "the mirror is damaged: delete it, and the next backup clones it again", "mirror", m.Path, "err", err)
		res.Mirror = MirrorDamaged
	default:
		unchecked = append(unchecked, fmt.Errorf("mirror: %w", err))
		res.Mirror = MirrorUnchecked
	}
	return res, errors.Join(unchecked...)
}

// checkSnapshot checks the snapshot at path: its checksum, then with full a
// test restore into dir. damaged reports whether err is about the snapshot's
// content, rather than about reading it, running git or the context.
func checkSnapshot(ctx context.Context, git *gitcmd.Runner, log *slog.Logger, path, dir string, full bool) (damaged bool, err error) {
	if err := bundle.CheckFile(ctx, path); err != nil {
		// Reading fails with an *fs.PathError. CheckPack's own errors are
		// about what it read.
		_, readErr := errors.AsType[*fs.PathError](err)
		return !readErr && ctx.Err() == nil, err
	}
	if !full {
		return false, nil
	}
	// A leftover would make the clone fail, and blame the snapshot.
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	err = testRestore(ctx, git, log, path, dir)
	return gitcmd.ExitCode(err) > 0 && ctx.Err() == nil && !outsideCause(err), err
}

// mirrorDamaged reports whether err, from mirror.Verify, says the mirror's
// objects are damaged, rather than that they couldn't be checked.
func mirrorDamaged(err error) bool {
	return errors.Is(err, mirror.ErrDamaged) || gitcmd.ExitCode(err) > 0 && !outsideCause(err)
}

// outsideCause reports whether git's error names a cause outside the data,
// such as a full disk. git fails with the same exit status for those as for
// damage, and only its message tells them apart: in English, since gitcmd
// sets LC_ALL=C, and in the words of the system's C library.
func outsideCause(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, cause := range []string{
		"no space left on device",
		"quota exceeded", // glibc's "disk quota", macOS's "disc quota", musl's plain "quota"
		"permission denied",
		"read-only file system",
		"out of memory", "cannot allocate memory",
		"too many open files",
		"died of signal", // a helper git started, such as index-pack, was killed
	} {
		if strings.Contains(msg, cause) {
			return true
		}
	}
	return false
}

// timed runs f under the repository's timeout, if it has one.
func (j *Job) timed(ctx context.Context, f func(context.Context) error) error {
	if j.Repo.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, j.Repo.Timeout)
		defer cancel()
	}
	return f(ctx)
}
