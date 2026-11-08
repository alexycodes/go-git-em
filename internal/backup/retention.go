package backup

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// Snapshot files are named after their UTC creation time, to the millisecond:
// 20261101T230000.000Z.bundle. The names have one length and no colons, so
// they sort in time order and work on any file system.
const nameLayout = "20060102T150405.000Z"

var (
	snapshotRe = regexp.MustCompile(`^\d{8}T\d{6}\.\d{3}Z\.bundle$`)
	// A snapshot being written, or left over by an interrupted run. The
	// .lock is git bundle create's own temporary file.
	tempRe = regexp.MustCompile(`^\.(\d{8}T\d{6}\.\d{3}Z\.bundle)\.tmp(\.lock)?$`)
)

func snapshotName(t time.Time) string {
	return t.UTC().Format(nameLayout) + ".bundle"
}

// isSnapshot reports whether name is a snapshot's: it must also be a real
// time, or it wouldn't be one of ours.
func isSnapshot(name string) bool {
	if !snapshotRe.MatchString(name) {
		return false
	}
	_, err := time.Parse(nameLayout, strings.TrimSuffix(name, ".bundle"))
	return err == nil
}

// isTemp reports whether name is a temporary file of a snapshot.
func isTemp(name string) bool {
	m := tempRe.FindStringSubmatch(name)
	return m != nil && isSnapshot(m[1])
}

// toDelete returns the snapshots to delete so that only the keep latest
// remain. keep 0 keeps them all.
func toDelete(names []string, keep int) []string {
	if keep <= 0 || len(names) <= keep {
		return nil
	}
	sorted := slices.Sorted(slices.Values(names))
	return sorted[:len(sorted)-keep]
}
