package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alexycodes/go-git-em/internal/config"
	"github.com/alexycodes/go-git-em/internal/schedule"
)

// runDaemon backs up each repository whenever its schedule is due, and at once
// with runOnStart, at most parallel at a time and in the order they fell due:
// the config's, for those due together. A repository whose previous backup is
// still running, or still queued, skips that time. Once ctx is done, it
// cancels the backups in progress, waits for them, and returns exitOK: being
// stopped is how a daemon ends. A schedule that doesn't parse, which
// config.Resolve rules out, returns exitUsage.
func runDaemon(ctx context.Context, logger *slog.Logger, repos []config.Repository, parallel int, runOnStart bool, runBackup backupFunc) int {
	logs := make([]*slog.Logger, len(repos))
	scheds := make([]schedule.Schedule, len(repos))
	for i, repo := range repos {
		logs[i] = repoLogger(logger, repo)
		s, err := schedule.Parse(repo.Schedule)
		if err != nil { // config.Resolve has already checked it
			logs[i].ErrorContext(ctx, "invalid schedule", "schedule", repo.Schedule, "err", err)
			return exitUsage
		}
		scheds[i] = s
	}
	start := time.Now()
	for i, repo := range repos {
		logs[i].InfoContext(ctx, "scheduled", "schedule", repo.Schedule, "next", scheds[i].Next(start))
	}

	const (
		idle int32 = iota
		queued
		running
	)
	state := make([]atomic.Int32, len(repos))
	type item struct {
		i       int
		startup bool // the backup at startup
	}
	// Holds each repository at most once, since only an idle one is queued, so
	// a send never blocks Run.
	queue := make(chan item, len(repos))
	var startupLeft atomic.Int64 // the backups at startup that haven't ended
	var wg sync.WaitGroup
	for range min(parallel, len(repos)) {
		wg.Go(func() {
			for it := range queue {
				state[it.i].Store(running)
				backupOne(ctx, logs[it.i], repos[it.i], runBackup) // skips it once stopped
				state[it.i].Store(idle)                            // first: a due time from now on queues it
				if it.startup {
					startupLeft.Add(-1)
				}
			}
		})
	}
	// enqueue runs on this goroutine only, at start and from Run, so nothing
	// else takes a repository out of idle.
	enqueue := func(due []int, startup bool) {
		for _, i := range due {
			var reason string
			switch state[i].Load() {
			case queued: // the -parallel workers are all busy with others
				reason = "previous run still queued"
			case running:
				reason = "previous run still running"
			default:
				state[i].Store(queued)
				queue <- item{i, startup}
				continue
			}
			// While the backups at startup drain, a skip says nothing of the
			// schedule or -parallel: it's expected after a restart.
			level := slog.LevelWarn
			if startupLeft.Load() > 0 {
				level = slog.LevelInfo
			}
			logs[i].Log(ctx, level, "backup", "result", "skipped", "reason", reason)
		}
	}

	if runOnStart {
		all := make([]int, len(repos))
		for i := range all {
			all[i] = i
		}
		startupLeft.Store(int64(len(repos)))
		enqueue(all, true)
	}
	schedule.Run(ctx, scheds, start, func(due []int) { enqueue(due, false) })
	logger.InfoContext(ctx, "stopping")
	close(queue) // Run has returned: nothing more is queued
	wg.Wait()
	return stopped(ctx, logger)
}

// stopped logs that the daemon has stopped, and returns its exit code.
func stopped(ctx context.Context, logger *slog.Logger) int {
	logger.InfoContext(ctx, "stopped")
	return exitOK
}
