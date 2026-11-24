package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/alexycodes/go-git-em/internal/backup"
	"github.com/alexycodes/go-git-em/internal/config"
)

// checkFunc checks repo's backups, logging what it finds to log.
type checkFunc func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.CheckResult, error)

// runCheck checks every repository's backups, at most parallel at a time and
// in the config's order. It logs one line per repository and a summary, and
// returns exitFail if anything is damaged, couldn't be checked, was
// interrupted, or had its output locked.
func runCheck(ctx context.Context, logger *slog.Logger, repos []config.Repository, parallel int, check checkFunc) int {
	count := forEach(repos, parallel, func(repo config.Repository) outcome {
		return checkOne(ctx, repoLogger(logger, repo), repo, check)
	})
	if n := count[interrupted]; n > 0 {
		logger.ErrorContext(ctx, "interrupted", "unfinished", n, "repositories", len(repos))
	}
	if n := count[damaged]; n > 0 {
		logger.ErrorContext(ctx, "some backups are damaged", "damaged", n, "repositories", len(repos))
	}
	if n := count[failed]; n > 0 {
		logger.ErrorContext(ctx, "some backups couldn't be checked", "failed", n, "repositories", len(repos))
	}
	if n := count[locked]; n > 0 {
		logger.ErrorContext(ctx, "some outputs were locked by another go-git-em", "locked", n, "repositories", len(repos))
	}
	if count[succeeded] < int64(len(repos)) {
		return exitFail
	}
	return exitOK
}

// checkOne checks one repository's backups and logs the result to log, a
// repoLogger. A repository with no snapshot gets a warning, but succeeds:
// there's nothing damaged.
func checkOne(ctx context.Context, log *slog.Logger, repo config.Repository, check checkFunc) outcome {
	if ctx.Err() != nil {
		log.DebugContext(ctx, "check", "result", "skipped", "reason", "interrupted")
		return interrupted
	}

	start := time.Now()
	res, err := check(ctx, repo, log)
	if errors.Is(err, backup.ErrLocked) {
		log.WarnContext(ctx, "check", "result", "skipped", "reason", backup.ErrLocked.Error())
		return locked
	}
	o, level, result := succeeded, slog.LevelInfo, "intact"
	switch {
	// Damage found before a signal or alongside what couldn't be checked
	// still counts as damage. err says what else happened.
	case res.Damaged > 0 || res.Mirror == backup.MirrorDamaged:
		o, level, result = damaged, slog.LevelError, "damaged"
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		o, level, result = interrupted, slog.LevelWarn, "interrupted"
	case err != nil:
		o, level, result = failed, slog.LevelError, "failed"
	case res.Snapshots == 0:
		level, result = slog.LevelWarn, "empty"
	}
	attrs := []any{"result", result, "snapshots", res.Snapshots, "damaged", res.Damaged}
	if res.Mirror != "" {
		attrs = append(attrs, "mirror", string(res.Mirror))
	}
	attrs = append(attrs, "duration", time.Since(start).Round(time.Millisecond))
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	log.Log(ctx, level, "check", attrs...)
	return o
}
