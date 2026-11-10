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
// any failures, and returns exitFail if any backup failed or was interrupted.
func runOnce(ctx context.Context, logger *slog.Logger, repos []config.Repository, parallel int, runBackup backupFunc) int {
	queue := make(chan config.Repository)
	var nFailed, nInterrupted atomic.Int64
	var wg sync.WaitGroup
	for range min(parallel, len(repos)) {
		wg.Go(func() {
			for repo := range queue {
				switch backupOne(ctx, logger, repo, runBackup) {
				case failed:
					nFailed.Add(1)
				case interrupted:
					nInterrupted.Add(1)
				}
			}
		})
	}
	for _, repo := range repos {
		queue <- repo
	}
	close(queue)
	wg.Wait()

	if n := nInterrupted.Load(); n > 0 {
		logger.ErrorContext(ctx, "interrupted", "unfinished", n, "repositories", len(repos))
	}
	if n := nFailed.Load(); n > 0 {
		logger.ErrorContext(ctx, "some backups failed", "failed", n, "repositories", len(repos))
	}
	if nInterrupted.Load()+nFailed.Load() > 0 {
		return exitFail
	}
	return exitOK
}

type outcome int

const (
	succeeded outcome = iota
	failed
	interrupted // cancelled, or never started because of it
)

// backupOne runs one backup and logs its result.
func backupOne(ctx context.Context, logger *slog.Logger, repo config.Repository, runBackup backupFunc) outcome {
	log := logger.With("output", repo.Output, "input", repo.Input) // the logger hides credentials
	// Backups still queued at a signal aren't worth an error each: the
	// summary counts them.
	if ctx.Err() != nil {
		log.DebugContext(ctx, "backup", "result", "skipped", "reason", "interrupted")
		return interrupted
	}

	start := time.Now()
	res, err := runBackup(ctx, repo, log)
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
