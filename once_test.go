package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alexycodes/go-git-em/internal/backup"
	"github.com/alexycodes/go-git-em/internal/config"
)

func repos(n int) []config.Repository {
	var rs []config.Repository
	for i := range n {
		rs = append(rs, config.Repository{Input: fmt.Sprintf("https://example.com/%d.git", i), Output: fmt.Sprintf("/backups/%d", i)})
	}
	return rs
}

func TestRunOnceParallel(t *testing.T) {
	for _, parallel := range []int{1, 3} {
		var mu sync.Mutex
		running, most, ran := 0, 0, 0
		release := make(chan struct{})
		fake := func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
			mu.Lock()
			running++
			most = max(most, running)
			ran++
			mu.Unlock()
			<-release
			mu.Lock()
			running--
			mu.Unlock()
			return backup.Result{Status: backup.Unchanged}, nil
		}
		var logs bytes.Buffer
		done := make(chan int)
		go func() {
			done <- runOnce(t.Context(), newLogger(&logs, slog.LevelInfo, "text", nil), repos(6), parallel, fake)
		}()

		// Once parallel backups run, give the others time to start too, if
		// nothing held them back. Then let them all finish.
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			n := running
			mu.Unlock()
			if n == parallel || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		close(release)
		code := <-done
		if code != exitOK || ran != 6 || most != parallel {
			t.Errorf("parallel %d: exit code %d, %d runs, at most %d at once; want %d, 6, %d", parallel, code, ran, most, exitOK, parallel)
		}
		if n := strings.Count(logs.String(), "msg=backup "); n != 6 {
			t.Errorf("parallel %d: %d log lines, want 6:\n%s", parallel, n, logs.String())
		}
	}
}

func TestRunOnceLogLines(t *testing.T) {
	results := map[string]struct {
		res backup.Result
		err error
	}{
		"/backups/0": {backup.Result{Status: backup.Created, Snapshot: "20261101T230000.000Z.bundle", Pruned: []string{"a", "b"}}, nil},
		"/backups/1": {backup.Result{Status: backup.Unchanged}, nil},
		"/backups/2": {backup.Result{Status: backup.Empty}, nil},
		"/backups/3": {backup.Result{}, errors.New("git fetch: exit status 128")},
		// Failed after its snapshot was written: both are reported.
		"/backups/4": {backup.Result{Status: backup.Created, Snapshot: "20261101T230001.000Z.bundle"}, errors.New("retention: permission denied")},
	}
	fake := func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
		log.WarnContext(ctx, "from the job")
		r := results[repo.Output]
		return r.res, r.err
	}
	var logs bytes.Buffer
	if code := runOnce(t.Context(), newLogger(&logs, slog.LevelInfo, "text", nil), repos(5), 2, fake); code != exitFail {
		t.Errorf("exit code = %d, want %d", code, exitFail)
	}
	for _, want := range []string{
		`level=INFO msg=backup output=/backups/0 input=https://example.com/0.git result=created snapshot=20261101T230000.000Z.bundle pruned=2 duration=`,
		`level=INFO msg=backup output=/backups/1 input=https://example.com/1.git result=unchanged pruned=0 duration=`,
		`level=INFO msg=backup output=/backups/2 input=https://example.com/2.git result=empty pruned=0 duration=`,
		`level=ERROR msg=backup output=/backups/3 input=https://example.com/3.git result=failed pruned=0 duration=`,
		`err="git fetch: exit status 128"`,
		`level=ERROR msg=backup output=/backups/4 input=https://example.com/4.git result=failed snapshot=20261101T230001.000Z.bundle pruned=0 duration=`,
		// The job's own warnings say which repository they're about.
		`level=WARN msg="from the job" output=/backups/3 input=https://example.com/3.git`,
		`level=ERROR msg="some backups failed" failed=2 repositories=5`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
}

// Once cancelled, no backup starts. Each one skipped gets a debug line, and the
// summary counts them.
func TestRunOnceCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var started atomic.Int64
	fake := func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
		started.Add(1)
		return backup.Result{}, nil
	}
	var logs bytes.Buffer
	if code := runOnce(ctx, newLogger(&logs, slog.LevelDebug, "text", nil), repos(4), 2, fake); code != exitFail {
		t.Errorf("exit code = %d, want %d", code, exitFail)
	}
	if n := started.Load(); n != 0 {
		t.Errorf("%d backups started after cancellation", n)
	}
	if n := strings.Count(logs.String(), `level=DEBUG msg=backup output=`); n != 4 || strings.Count(logs.String(), `result=skipped reason=interrupted`) != 4 {
		t.Errorf("want 4 skipped lines:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "result=failed") {
		t.Errorf("a skipped backup logged a failure:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `level=ERROR msg=interrupted unfinished=4 repositories=4`) {
		t.Errorf("no interrupted line:\n%s", logs.String())
	}
}

// git's errors quote their arguments, inputs included: the logs must hide
// the credentials wherever they appear.
func TestLoggerHidesCredentials(t *testing.T) {
	secret := "https://me:correcthorsebattery@example.com/a.git"
	repos := []config.Repository{
		{Input: secret, Output: "/backups/a"},
		{Input: "https://me:backup@example.com/r.git", Output: "/backups/r"},               // a word: not hidden alone
		{Input: "https://ghp_0123456789abcdefghij@github.com/b.git", Output: "/backups/b"}, // a token as the user name
		{Input: "ssh://git:pw@example.com/c.git", Output: "/backups/c"},                    // too short to hide alone
		{Input: "https://me:abc:defgh@example.com/e.git", Output: "/backups/e"},            // a ":" in the password
		{Input: "https://alexycodes@github.com/alexycodes/f.git", Output: "/backups/alexycodes"},
		{Input: "https://example.com/d.git", Output: "/backups/d"},
	}
	for _, format := range []string{"text", "json"} {
		var logs bytes.Buffer
		logger := newLogger(&logs, slog.LevelInfo, format, credentials(repos))
		logger.Error("git clone -- "+secret+" failed", "err", errors.New("git clone -- "+secret+": fatal"), "input", secret)
		// The end of git's stderr can start inside a URL.
		logger.Error("tail", "err", errors.New("...\ncorrecthorsebattery@example.com/a.git"))
		logger.Error("short", "err", errors.New("fatal: me:backup@example.com/r.git: not found; backup is a word"))
		logger.Error("token", "err", errors.New("fatal: https://ghp_0123456789abcdefghij@github.com/b.git/: not found"))
		logger.Error("token cut", "err", errors.New("...ghp_0123456789abcdefghij@github.com"))
		logger.Error("ssh", "err", errors.New("ssh://git:pw@example.com/c.git: refused; pwd is fine, rpw@example.org too"))
		logger.Error("ssh without scheme", "err", errors.New("connecting to git:pw@example.com failed"))
		logger.Error("colon", "err", errors.New("...me:abc:defgh@example.com"))
		logger.Error("user name", "output", "/backups/alexycodes", "err", errors.New("see github.com/alexycodes"))
		out := logs.String()
		for _, leak := range []string{"correcthorsebattery", ":backup@", "ghp_0123456789abcdefghij", ":pw@", "abc:defgh"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s logs hold %q:\n%s", format, leak, out)
			}
		}
		for _, want := range []string{
			"https://xxxxx@example.com/a.git", "xxxxx@example.com/a.git", "https://xxxxx@github.com/b.git",
			"fatal: xxxxx@example.com/r.git", "backup is a word", // hidden only before "@<host>"
			"ssh://git:xxxxx@example.com/c.git", "git:xxxxx@example.com", "pwd is fine, rpw@example.org too",
			"/backups/alexycodes", "github.com/alexycodes", // a plain user name isn't a secret
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s logs lack %q:\n%s", format, want, out)
			}
		}
	}
	if credentials(repos[6:]) != nil {
		t.Error("credentials() isn't nil for inputs without credentials")
	}
}

// A signal that comes once every backup is done changes nothing.
func TestRunOnceLateSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var logs bytes.Buffer
	code := runOnce(ctx, newLogger(&logs, slog.LevelInfo, "text", nil), repos(1), 1, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
		cancel() // the signal, just as the only backup finishes
		return backup.Result{Status: backup.Unchanged}, nil
	})
	if code != exitOK || strings.Contains(logs.String(), "interrupted") {
		t.Errorf("exit code %d, logs:\n%s", code, logs.String())
	}
}

// A backup that failed before the signal counts as failed, not interrupted.
func TestRunOnceFailedThenInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var logs bytes.Buffer
	var mu sync.Mutex
	calls := 0
	code := runOnce(ctx, newLogger(&logs, slog.LevelInfo, "text", nil), repos(2), 1, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return backup.Result{}, errors.New("git fetch: exit status 128")
		}
		cancel()
		return backup.Result{}, ctx.Err()
	})
	if code != exitFail {
		t.Errorf("exit code = %d, want %d", code, exitFail)
	}
	for _, want := range []string{
		`level=ERROR msg=backup output=/backups/0 input=https://example.com/0.git result=failed`,
		`level=WARN msg=backup output=/backups/1 input=https://example.com/1.git result=interrupted pruned=0 duration=`,
		`msg=interrupted unfinished=1 repositories=2`, `msg="some backups failed" failed=1 repositories=2`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
}

// A repository's own timeout is a failure, though it's a context error too.
func TestRunOnceTimeoutIsAFailure(t *testing.T) {
	var logs bytes.Buffer
	code := runOnce(t.Context(), newLogger(&logs, slog.LevelInfo, "text", nil), repos(1), 1, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
		return backup.Result{}, fmt.Errorf("update the mirror: %w", context.DeadlineExceeded)
	})
	if code != exitFail || !strings.Contains(logs.String(), `msg="some backups failed" failed=1`) || strings.Contains(logs.String(), "interrupted") {
		t.Errorf("exit code %d, logs:\n%s", code, logs.String())
	}
}

func TestRunOnceOrder(t *testing.T) {
	var got []string
	var logs bytes.Buffer
	runOnce(t.Context(), newLogger(&logs, slog.LevelInfo, "text", nil), repos(5), 1, func(ctx context.Context, repo config.Repository, log *slog.Logger) (backup.Result, error) {
		got = append(got, repo.Output)
		return backup.Result{Status: backup.Unchanged}, nil
	})
	want := []string{"/backups/0", "/backups/1", "/backups/2", "/backups/3", "/backups/4"}
	if !slices.Equal(got, want) {
		t.Errorf("ran %q, want the config's order %q", got, want)
	}
}

// A signal the process was started to ignore, as nohup does with SIGHUP,
// stays ignored.
func TestStopSignals(t *testing.T) {
	// Under nohup, the test itself starts with SIGHUP ignored.
	if !signal.Ignored(syscall.SIGHUP) && !slices.Contains(stopSignals(), os.Signal(syscall.SIGHUP)) {
		t.Fatalf("stopSignals() = %v, want SIGHUP among them", stopSignals())
	}
	signal.Ignore(syscall.SIGHUP)
	t.Cleanup(func() { signal.Reset(syscall.SIGHUP) })
	if slices.Contains(stopSignals(), os.Signal(syscall.SIGHUP)) {
		t.Errorf("stopSignals() = %v with SIGHUP ignored", stopSignals())
	}
}
