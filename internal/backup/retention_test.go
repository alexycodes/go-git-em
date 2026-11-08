package backup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSnapshotName(t *testing.T) {
	paris := time.FixedZone("CET", 3600)
	tests := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC), "20261101T230000.000Z.bundle"},
		{time.Date(2026, 11, 2, 0, 30, 5, 123456789, paris), "20261101T233005.123Z.bundle"}, // UTC, milliseconds
		{time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC), "20260102T030405.006Z.bundle"},
	}
	for _, tt := range tests {
		got := snapshotName(tt.t)
		if got != tt.want {
			t.Errorf("snapshotName(%v) = %q, want %q", tt.t, got, tt.want)
		}
		if !isSnapshot(got) {
			t.Errorf("isSnapshot(%q) = false", got)
		}
		if !isTemp("." + got + ".tmp") {
			t.Errorf("isTemp(%q) = false", "."+got+".tmp")
		}
	}
}

func TestNamePatterns(t *testing.T) {
	for name, want := range map[string][2]bool{ // name → isSnapshot, isTemp
		"20261101T230000.000Z.bundle":            {true, false},
		".20261101T230000.000Z.bundle.tmp":       {false, true},
		".20261101T230000.000Z.bundle.tmp.lock":  {false, true}, // git bundle create's own temporary file
		"20261101T230000Z.bundle":                {false, false},
		"20261101T230000.000Z.bundle.bak":        {false, false},
		"x20261101T230000.000Z.bundle":           {false, false},
		"20261101T230000.000Z.bundle.tmp":        {false, false},
		".notes.tmp":                             {false, false},
		".20261101T230000.000Z.bundle.tmp.other": {false, false},
		// The right shape, but not a time: not ours.
		"20261340T999999.000Z.bundle":      {false, false},
		".20261340T999999.000Z.bundle.tmp": {false, false},
	} {
		if got := isSnapshot(name); got != want[0] {
			t.Errorf("isSnapshot(%q) = %v, want %v", name, got, want[0])
		}
		if got := isTemp(name); got != want[1] {
			t.Errorf("isTemp(%q) = %v, want %v", name, got, want[1])
		}
	}
}

func TestToDelete(t *testing.T) {
	names := []string{ // not in order
		"20261103T000000.000Z.bundle",
		"20261101T000000.000Z.bundle",
		"20261104T000000.000Z.bundle",
		"20261102T000000.000Z.bundle",
	}
	tests := []struct {
		keep int
		want []string
	}{
		{0, nil}, // keep all
		{1, []string{"20261101T000000.000Z.bundle", "20261102T000000.000Z.bundle", "20261103T000000.000Z.bundle"}},
		{3, []string{"20261101T000000.000Z.bundle"}},
		{4, nil},
		{10, nil},
	}
	for _, tt := range tests {
		if got := toDelete(names, tt.keep); !slices.Equal(got, tt.want) {
			t.Errorf("toDelete(keep %d) = %q, want %q", tt.keep, got, tt.want)
		}
	}
	if toDelete(nil, 2) != nil {
		t.Errorf("toDelete(nil, 2) isn't nil")
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"20261101T000000.000Z.bundle", "20261103T000000.000Z.bundle"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 20261102 is listed but already gone: deleting it isn't this run's doing.
	snapshots := []string{"20261101T000000.000Z.bundle", "20261102T000000.000Z.bundle", "20261103T000000.000Z.bundle"}
	pruned, err := prune(dir, snapshots, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"20261101T000000.000Z.bundle"}; !slices.Equal(pruned, want) {
		t.Errorf("pruned %q, want %q", pruned, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "20261103T000000.000Z.bundle" {
		t.Errorf("left %v, %v; want only the newest snapshot", entries, err)
	}
}
