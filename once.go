package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alexycodes/go-git-em/internal/backup"
	"github.com/alexycodes/go-git-em/internal/config"
)

// backupFunc runs one backup of repo, logging its warnings to log.
type backupFunc func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error)

// runOnce backs up every repository once, at most parallel at a time and in
// the config's order. It logs one line per backup it runs and a summary of
// any failures, and returns exitFail if any backup failed, was interrupted, or
// found its output locked.
func runOnce(ctx context.Context, logger *slog.Logger, repos []config.Repository, parallel int, runBackup backupFunc) int {
	queue := make(chan config.Repository)
	var count [numOutcomes]atomic.Int64 // backups by outcome
	var wg sync.WaitGroup
	for range min(parallel, len(repos)) {
		wg.Go(func() {
			for repo := range queue {
				count[backupOne(ctx, repoLogger(logger, repo), repo, runBackup)].Add(1)
			}
		})
	}
	for _, repo := range repos {
		queue <- repo
	}
	close(queue)
	wg.Wait()

	if n := count[interrupted].Load(); n > 0 {
		logger.ErrorContext(ctx, "interrupted", "unfinished", n, "repositories", len(repos))
	}
	if n := count[failed].Load(); n > 0 {
		logger.ErrorContext(ctx, "some backups failed", "failed", n, "repositories", len(repos))
	}
	if n := count[locked].Load(); n > 0 {
		logger.ErrorContext(ctx, "some outputs were locked by another go-git-em", "locked", n, "repositories", len(repos))
	}
	if count[succeeded].Load() < int64(len(repos)) {
		return exitFail
	}
	return exitOK
}

type outcome int

const (
	succeeded outcome = iota
	failed
	interrupted // cancelled, or never started because of it
	locked      // another go-git-em had the output, so this run didn't back it up

	numOutcomes // sizes arrays indexed by outcome
)

// repoLogger returns logger with the attributes that say which repository a
// line is about.
func repoLogger(logger *slog.Logger, repo config.Repository) *slog.Logger {
	return logger.With("output", repo.Output, "input", repo.Input) // the logger hides credentials
}

// backupOne runs one backup and logs its result to log, a repoLogger.
func backupOne(ctx context.Context, log *slog.Logger, repo config.Repository, runBackup backupFunc) outcome {
	// Backups still queued at a signal aren't worth an error each: in -once
	// mode the summary counts them, and a daemon is stopping anyway.
	if ctx.Err() != nil {
		log.DebugContext(ctx, "backup", "result", "skipped", "reason", "interrupted")
		return interrupted
	}

	start := time.Now()
	res, err := runBackup(ctx, repo, log)
	if errors.Is(err, backup.ErrLocked) {
		log.WarnContext(ctx, "backup", "result", "skipped", "reason", backup.ErrLocked.Error())
		return locked
	}
	o, level, result := succeeded, slog.LevelInfo, string(res.Status)
	switch {
	// Interrupted only if the signal is why it failed: a repository's own
	// timeout, also a context error, is a failure.
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		o, level, result = interrupted, slog.LevelWarn, "interrupted"
	case err != nil:
		o, level, result = failed, slog.LevelError, "failed"
	}
	attrs := []any{"result", result}
	if res.Snapshot != "" {
		attrs = append(attrs, "snapshot", res.Snapshot)
	}
	attrs = append(attrs, "pruned", len(res.Pruned), "duration", time.Since(start).Round(time.Millisecond))
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	log.Log(ctx, level, "backup", attrs...)
	return o
}
