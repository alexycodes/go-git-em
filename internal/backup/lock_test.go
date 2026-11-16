//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows

package backup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// While another process holds the output's lock, a run touches none of its
// files, not even a temporary file that is that process's work in progress.
func TestRunLocked(t *testing.T) {
	e := newEnv(t)
	tmp := e.leftover(t)
	unlock, err := lock(filepath.Join(e.job.Repo.Output, LockFile)) // another open file, as another process's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock) // if the test stops early
	if _, err := e.job.Run(t.Context()); !errors.Is(err, ErrLocked) {
		t.Fatalf("Run: %v, want %v", err, ErrLocked)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("the other process's temporary file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.job.Repo.Output, MirrorDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("mirror: %v, want none", err)
	}

	// Once it's released, runs go ahead, and each releases the lock in turn.
	unlock()
	e.run(t, Created)
	e.run(t, Unchanged)
}

// A lock that fails for another reason fails the run before it touches
// anything.
func TestRunLockFails(t *testing.T) {
	e := newEnv(t)
	tmp := e.leftover(t)
	if err := os.Mkdir(filepath.Join(e.job.Repo.Output, LockFile), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := e.job.Run(t.Context())
	if err == nil || errors.Is(err, ErrLocked) || errors.Is(err, errNoLocks) || !strings.Contains(err.Error(), "lock the output: ") {
		t.Fatalf("Run: %v, want a failed lock", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("temporary file: %v", err)
	}
}

// leftover writes a temporary snapshot file, as an interrupted run, or one in
// progress, leaves, and returns its path.
func (e *env) leftover(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(e.job.Repo.Output, SnapshotsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".20261101T220000.000Z.bundle.tmp")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return tmp
}

// checkLockError checks that lockError maps err to want, and to no other
// meaning.
func checkLockError(t *testing.T, err, want error) {
	t.Helper()
	got := lockError(err)
	if !errors.Is(got, want) || errors.Is(got, ErrLocked) != errors.Is(want, ErrLocked) || errors.Is(got, errNoLocks) != errors.Is(want, errNoLocks) {
		t.Errorf("lockError(%v) = %v, want %v", err, got, want)
	}
}
