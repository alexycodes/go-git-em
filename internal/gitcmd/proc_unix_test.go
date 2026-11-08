//go:build unix

package gitcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTimeoutKillsHelpers checks that a timeout leaves no process behind: a
// fake git starts two helpers, one of which ignores SIGTERM.
func TestTimeoutKillsHelpers(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	fakeGit := writeScript(t, dir, "git", `
sleep 300 &
echo $! >> "$PID_FILE"
sh -c 'trap "" TERM; exec sleep 300' &
echo $! >> "$PID_FILE"
wait
`)
	git := &Runner{Path: fakeGit, Env: testEnv(t, "PID_FILE="+pidFile), WaitDelay: 200 * time.Millisecond}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := git.Run(ctx, "", "fetch")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run returned after %v, want about 1.2s", elapsed)
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pids := strings.Fields(string(data))
	if len(pids) != 2 {
		t.Fatalf("fake git started %d helpers, want 2", len(pids))
	}
	for _, s := range pids {
		pid, err := strconv.Atoi(s)
		if err != nil {
			t.Fatal(err)
		}
		waitGone(t, pid)
	}
}

// waitGone fails the test if pid is still running shortly after.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if gone(pid) {
			return
		}
	}
	t.Errorf("helper %d is still running", pid)
}

// gone reports whether pid has exited. An exited process that nobody reaped
// yet counts as gone: in a container, PID 1 may never reap orphans.
func gone(pid int) bool {
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return true
		}
		// The state follows the command name, which is in parentheses.
		fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
		return len(fields) > 0 && fields[0] == "Z"
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
