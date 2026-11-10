package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alexycodes/go-git-em/internal/backup"
	"github.com/alexycodes/go-git-em/internal/config"
)

// The daemon tests run on synctest's fake clock, which moves on only when
// every goroutine is waiting, so their timings are exact.

func everySecond(n int) []config.Repository {
	rs := repos(n)
	for i := range rs {
		rs[i].Schedule = "@every 1s"
	}
	return rs
}

// daemon runs runDaemon until stop is called, which returns its exit code.
func daemon(t *testing.T, logs *bytes.Buffer, rs []config.Repository, parallel int, runOnStart bool, fake backupFunc) (stop func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	code := make(chan int, 1) // so runDaemon can return if the test fails first
	go func() {
		code <- runDaemon(ctx, newLogger(logs, slog.LevelDebug, "text", nil), rs, parallel, runOnStart, fake)
	}()
	return func() int {
		cancel()
		return <-code
	}
}

// ranAt records when each output was backed up, from the start of the test,
// and in which order.
type ranAt struct {
	mu    sync.Mutex
	start time.Time
	times map[string][]time.Duration
	order []string
}

func newRanAt() *ranAt { return &ranAt{start: time.Now(), times: make(map[string][]time.Duration)} }

func (r *ranAt) add(output string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.times[output] = append(r.times[output], time.Since(r.start))
	r.order = append(r.order, output)
}

func (r *ranAt) get(output string) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.times[output])
}

func TestRunDaemon(t *testing.T) {
	tests := []struct {
		runOnStart bool
		want       []time.Duration
	}{
		{true, []time.Duration{0, time.Second, 2 * time.Second}},
		{false, []time.Duration{time.Second, 2 * time.Second}},
	}
	for _, tt := range tests {
		synctest.Test(t, func(t *testing.T) {
			ran := newRanAt()
			next := time.Now().Add(time.Second).Format("2006-01-02T15:04:05.000Z07:00") // as slog writes it
			var logs bytes.Buffer
			stop := daemon(t, &logs, everySecond(2), 4, tt.runOnStart, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
				ran.add(repo.Output)
				return backup.Result{Status: backup.Unchanged}, nil
			})
			time.Sleep(2500 * time.Millisecond)
			if code := stop(); code != exitOK {
				t.Errorf("run on start %v: exit code %d, want %d", tt.runOnStart, code, exitOK)
			}
			for _, out := range []string{"/backups/0", "/backups/1"} {
				if got := ran.get(out); !slices.Equal(got, tt.want) {
					t.Errorf("run on start %v: %s ran at %v, want %v", tt.runOnStart, out, got, tt.want)
				}
			}
			for _, want := range []string{
				`level=INFO msg=scheduled output=/backups/0 input=https://example.com/0.git schedule="@every 1s" next=` + next + "\n",
				`level=INFO msg=backup output=/backups/1 input=https://example.com/1.git result=unchanged pruned=0 duration=0s`,
				"level=INFO msg=stopping\n",
				"level=INFO msg=stopped\n",
			} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("run on start %v: logs lack %q:\n%s", tt.runOnStart, want, logs.String())
				}
			}
		})
	}
}

// A repository due while its previous backup still runs skips that time, with
// a warning once the backup at startup is done.
func TestRunDaemonSkipsBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := newRanAt()
		var logs bytes.Buffer
		stop := daemon(t, &logs, everySecond(1), 4, true, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
			ran.add(repo.Output)
			if len(ran.get(repo.Output)) == 1 { // the backup at startup
				return backup.Result{Status: backup.Unchanged}, nil
			}
			select {
			case <-time.After(2500 * time.Millisecond):
				return backup.Result{Status: backup.Unchanged}, nil
			case <-ctx.Done():
				return backup.Result{}, fmt.Errorf("git fetch: %w", ctx.Err())
			}
		})
		// Runs at 0, then at 1s until 3.5s, skips 2s and 3s, runs again at 4s.
		time.Sleep(4500 * time.Millisecond)
		if code := stop(); code != exitOK {
			t.Errorf("exit code %d, want %d", code, exitOK)
		}
		if got, want := ran.get("/backups/0"), []time.Duration{0, time.Second, 4 * time.Second}; !slices.Equal(got, want) {
			t.Errorf("ran at %v, want %v", got, want)
		}
		for line, want := range map[string]int{
			`level=WARN msg=backup output=/backups/0 input=https://example.com/0.git result=skipped reason="previous run still running"`: 2,
			`result=unchanged pruned=0 duration=2.5s`: 1,
			// Stopping cancels the backup in progress.
			`level=WARN msg=backup output=/backups/0 input=https://example.com/0.git result=interrupted pruned=0 duration=500ms err="git fetch: context canceled"`: 1,
		} {
			if n := strings.Count(logs.String(), line); n != want {
				t.Errorf("%d lines with %q, want %d:\n%s", n, line, want, logs.String())
			}
		}
	})
}

func TestRunDaemonParallel(t *testing.T) {
	for _, parallel := range []int{1, 2} {
		synctest.Test(t, func(t *testing.T) {
			var c counting
			var logs bytes.Buffer
			stop := daemon(t, &logs, everySecond(3), parallel, true, c.fake(func() { time.Sleep(100 * time.Millisecond) }))
			time.Sleep(500 * time.Millisecond)
			if code := stop(); code != exitOK || c.most != parallel || c.ran != 3 {
				t.Errorf("parallel %d: exit code %d, %d runs, at most %d at once; want %d, 3, %d", parallel, code, c.ran, c.most, exitOK, parallel)
			}
		})
	}
}

// Backups due together run in the config's order, at start and on their
// schedule alike.
func TestRunDaemonOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := newRanAt()
		var logs bytes.Buffer
		stop := daemon(t, &logs, everySecond(3), 1, true, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
			ran.add(repo.Output)
			time.Sleep(100 * time.Millisecond)
			return backup.Result{Status: backup.Unchanged}, nil
		})
		time.Sleep(1500 * time.Millisecond)
		stop()
		want := []string{"/backups/0", "/backups/1", "/backups/2", "/backups/0", "/backups/1", "/backups/2"}
		if !slices.Equal(ran.order, want) {
			t.Errorf("ran %v, want %v", ran.order, want)
		}
	})
}

// Stopping cancels the backup in progress, and starts none of those waiting
// for their turn.
func TestRunDaemonStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := newRanAt()
		var logs bytes.Buffer
		stop := daemon(t, &logs, everySecond(2), 1, true, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
			ran.add(repo.Output)
			<-ctx.Done()
			return backup.Result{}, fmt.Errorf("git fetch: %w", ctx.Err())
		})
		time.Sleep(1500 * time.Millisecond)
		if code := stop(); code != exitOK {
			t.Errorf("exit code %d, want %d", code, exitOK)
		}
		if got, want := ran.get("/backups/0"), []time.Duration{0}; !slices.Equal(got, want) {
			t.Errorf("/backups/0 ran at %v, want %v", got, want)
		}
		if got := ran.get("/backups/1"); got != nil {
			t.Errorf("/backups/1 ran at %v, want never", got)
		}
		for line, want := range map[string]int{
			// At 1s, one is running and the other still waits for its turn, both
			// since startup, so the skips are only INFO.
			`level=INFO msg=backup output=/backups/0 input=https://example.com/0.git result=skipped reason="previous run still running"`:                          1,
			`level=INFO msg=backup output=/backups/1 input=https://example.com/1.git result=skipped reason="previous run still queued"`:                           1,
			`level=WARN msg=backup output=/backups/0 input=https://example.com/0.git result=interrupted pruned=0 duration=1.5s err="git fetch: context canceled"`: 1,
			`level=DEBUG msg=backup output=/backups/1 input=https://example.com/1.git result=skipped reason=interrupted`:                                          1,
		} {
			if n := strings.Count(logs.String(), line); n != want {
				t.Errorf("%d lines with %q, want %d:\n%s", n, line, want, logs.String())
			}
		}
		// The cancelled backup and "stopping" race to the log, but "stopped"
		// waits for the backup.
		stoppingAt, stoppedAt := strings.Index(logs.String(), "msg=stopping"), strings.Index(logs.String(), "msg=stopped")
		if stoppingAt < 0 || stoppedAt < stoppingAt || stoppedAt < strings.Index(logs.String(), "result=interrupted") {
			t.Errorf("want stopping and the interrupted backup, then stopped:\n%s", logs.String())
		}
	})
}

func TestRunDaemonInvalidSchedule(t *testing.T) {
	rs := everySecond(1)
	rs[0].Schedule = "@fortnightly"
	var logs bytes.Buffer
	code := runDaemon(t.Context(), newLogger(&logs, slog.LevelInfo, "text", nil), rs, 1, true, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
		t.Error("a backup ran")
		return backup.Result{}, nil
	})
	if code != exitUsage || !strings.Contains(logs.String(), `level=ERROR msg="invalid schedule" output=/backups/0`) {
		t.Errorf("exit code %d, logs:\n%s", code, logs.String())
	}
}

// A failed backup is logged, and its repository is backed up again when next
// due: the daemon goes on.
func TestRunDaemonFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := newRanAt()
		var logs bytes.Buffer
		stop := daemon(t, &logs, everySecond(1), 1, true, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
			ran.add(repo.Output)
			return backup.Result{}, errors.New("git fetch: exit status 128")
		})
		time.Sleep(2500 * time.Millisecond)
		if code := stop(); code != exitOK {
			t.Errorf("exit code %d, want %d", code, exitOK)
		}
		if got, want := ran.get("/backups/0"), []time.Duration{0, time.Second, 2 * time.Second}; !slices.Equal(got, want) {
			t.Errorf("ran at %v, want %v", got, want)
		}
		if n := strings.Count(logs.String(), `level=ERROR msg=backup output=/backups/0 input=https://example.com/0.git result=failed`); n != 3 {
			t.Errorf("%d failures logged, want 3:\n%s", n, logs.String())
		}
	})
}

// While the backups at startup drain, skips are INFO, even for a repository
// whose own backup at startup is done.
func TestRunDaemonStartupDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := newRanAt()
		var logs bytes.Buffer
		stop := daemon(t, &logs, everySecond(2), 1, true, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
			ran.add(repo.Output)
			if repo.Output == "/backups/1" && len(ran.get(repo.Output)) == 1 {
				time.Sleep(2500 * time.Millisecond) // a first clone, say
			}
			return backup.Result{Status: backup.Unchanged}, nil
		})
		// /backups/0 is done at once, queued at 1s behind /backups/1, and still
		// queued at 2s. The drain ends at 2.5s.
		time.Sleep(2700 * time.Millisecond)
		stop()
		for line, want := range map[string]int{
			`level=INFO msg=backup output=/backups/0 input=https://example.com/0.git result=skipped reason="previous run still queued"`:  1,
			`level=INFO msg=backup output=/backups/1 input=https://example.com/1.git result=skipped reason="previous run still running"`: 2,
			`level=WARN`: 0,
		} {
			if n := strings.Count(logs.String(), line); n != want {
				t.Errorf("%d lines with %q, want %d:\n%s", n, line, want, logs.String())
			}
		}
	})
}
