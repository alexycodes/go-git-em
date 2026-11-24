package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/alexycodes/go-git-em/internal/backup"
	"github.com/alexycodes/go-git-em/internal/config"
)

// Each finding has its line and level, and the summary counts them.
func TestRunCheckLogLines(t *testing.T) {
	results := map[string]struct {
		res backup.CheckResult
		err error
	}{
		"/backups/0": {res: backup.CheckResult{Snapshots: 2, Mirror: backup.MirrorIntact}},
		"/backups/1": {res: backup.CheckResult{Snapshots: 2, Damaged: 1, Mirror: backup.MirrorIntact}},
		"/backups/2": {res: backup.CheckResult{Snapshots: 2, Mirror: backup.MirrorDamaged}},
		"/backups/3": {res: backup.CheckResult{Snapshots: 1, Mirror: backup.MirrorUnchecked}, err: errors.New("mirror: unreadable")},
		"/backups/4": {res: backup.CheckResult{Mirror: backup.MirrorMissing}},
		"/backups/5": {err: fmt.Errorf("lock the output: %w", backup.ErrLocked)},
	}
	var logs bytes.Buffer
	code := runCheck(t.Context(), newLogger(&logs, slog.LevelInfo, "text", nil), repos(6), 2, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.CheckResult, error) {
		r := results[repo.Output]
		return r.res, r.err
	})
	if code != exitFail {
		t.Errorf("exit code %d, want %d", code, exitFail)
	}
	for _, want := range []string{
		`level=INFO msg=check output=/backups/0 input=https://example.com/0.git result=intact snapshots=2 damaged=0 mirror=intact duration=`,
		`level=ERROR msg=check output=/backups/1 input=https://example.com/1.git result=damaged snapshots=2 damaged=1 mirror=intact duration=`,
		`level=ERROR msg=check output=/backups/2 input=https://example.com/2.git result=damaged snapshots=2 damaged=0 mirror=damaged duration=`,
		`level=ERROR msg=check output=/backups/3 input=https://example.com/3.git result=failed snapshots=1 damaged=0 mirror=unchecked duration=`,
		`err="mirror: unreadable"`,
		`level=WARN msg=check output=/backups/4 input=https://example.com/4.git result=empty snapshots=0 damaged=0 mirror=missing duration=`,
		`level=WARN msg=check output=/backups/5 input=https://example.com/5.git result=skipped reason="another go-git-em is using this output"`,
		`level=ERROR msg="some backups are damaged" damaged=2 repositories=6`,
		`level=ERROR msg="some backups couldn't be checked" failed=1 repositories=6`,
		`level=ERROR msg="some outputs were locked by another go-git-em" locked=1 repositories=6`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
}

// Nothing damaged is exit 0, even with nothing backed up yet.
func TestRunCheckIntact(t *testing.T) {
	var logs bytes.Buffer
	code := runCheck(t.Context(), newLogger(&logs, slog.LevelInfo, "text", nil), repos(2), 1, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.CheckResult, error) {
		if repo.Output == "/backups/0" {
			return backup.CheckResult{Mirror: backup.MirrorMissing}, nil
		}
		return backup.CheckResult{Snapshots: 3, Mirror: backup.MirrorIntact}, nil
	})
	if code != exitOK || strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("exit code %d, logs:\n%s", code, logs.String())
	}
}

// A check the signal cut short, and those it kept from starting, are
// unfinished. Damage found before the signal still counts.
func TestRunCheckInterrupted(t *testing.T) {
	for _, found := range []int{0, 1} {
		ctx, cancel := context.WithCancel(t.Context())
		var logs bytes.Buffer
		code := runCheck(ctx, newLogger(&logs, slog.LevelInfo, "text", nil), repos(3), 1, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.CheckResult, error) {
			cancel()
			return backup.CheckResult{Snapshots: 1, Damaged: found}, ctx.Err()
		})
		first := `level=WARN msg=check output=/backups/0 input=https://example.com/0.git result=interrupted snapshots=1 damaged=0 duration=`
		unfinished := `level=ERROR msg=interrupted unfinished=3 repositories=3`
		if found > 0 {
			first = `level=ERROR msg=check output=/backups/0 input=https://example.com/0.git result=damaged snapshots=1 damaged=1 duration=`
			unfinished = `level=ERROR msg=interrupted unfinished=2 repositories=3`
		}
		if code != exitFail || !strings.Contains(logs.String(), first) || !strings.Contains(logs.String(), unfinished) || !strings.Contains(logs.String(), `err="context canceled"`) {
			t.Errorf("damaged %d: exit code %d, logs:\n%s", found, code, logs.String())
		}
	}
}
