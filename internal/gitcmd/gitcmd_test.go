package gitcmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexycodes/go-git-em/internal/shtest"
)

// testEnv is a git environment that ignores the developer's own configuration.
func testEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
	}, extra...)
}

func TestRunOutput(t *testing.T) {
	git := &Runner{Env: testEnv(t)}
	dir := t.TempDir()
	if _, err := git.Run(t.Context(), dir, "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	out, err := git.Run(t.Context(), dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "true\n" {
		t.Errorf("stdout = %q, want %q", out, "true\n")
	}
}

func TestRunError(t *testing.T) {
	git := &Runner{Env: testEnv(t)}
	dir := t.TempDir()
	if _, err := git.Run(t.Context(), dir, "init", "-q"); err != nil {
		t.Fatal(err)
	}

	_, err := git.Run(t.Context(), dir, "rev-parse", "--verify", "--", "no-such-ref")
	var gitErr *Error
	if !errors.As(err, &gitErr) {
		t.Fatalf("err = %v, want an *Error", err)
	}
	if code := gitErr.ExitCode(); code != 128 {
		t.Errorf("ExitCode() = %d, want 128", code)
	}
	if !strings.HasPrefix(gitErr.Stderr, "fatal: ") {
		t.Errorf("Stderr = %q, want git's English error", gitErr.Stderr)
	}
	want := "git rev-parse --verify -- no-such-ref: exit status 128: fatal: "
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Error() = %q, want it to start with %q", err, want)
	}

	wrapped := fmt.Errorf("context: %w", err)
	for err, want := range map[error]int{nil: 0, wrapped: 128, errors.New("other"): -1} {
		if got := ExitCode(err); got != want {
			t.Errorf("ExitCode(%v) = %d, want %d", err, got, want)
		}
	}
}

func TestRunMissingGit(t *testing.T) {
	git := &Runner{Path: filepath.Join(t.TempDir(), "git"), Env: testEnv(t)}
	_, err := git.Run(t.Context(), "", "version")
	var gitErr *Error
	if !errors.As(err, &gitErr) || gitErr.ExitCode() != -1 {
		t.Fatalf("err = %v, want an *Error with exit code -1", err)
	}
	// On Windows, exec looks the path up with each executable extension.
	missing := errors.Is(err, os.ErrNotExist) || runtime.GOOS == "windows" && errors.Is(err, exec.ErrNotFound)
	if !missing {
		t.Errorf("err = %v, want it to wrap os.ErrNotExist", err)
	}
}

func TestVersion(t *testing.T) {
	v, err := (&Runner{Env: testEnv(t)}).Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Apple's git adds a suffix: "2.39.5 (Apple Git-154)".
	if v == "" || v[0] < '0' || v[0] > '9' || strings.Contains(v, "\n") {
		t.Errorf("Version() = %q, want it to start with the version number", v)
	}
	// The git that runs the tests is a supported one.
	if old, known := TooOld(v); old || !known {
		t.Errorf("TooOld(%q) = %v, %v; want false, true", v, old, known)
	}
}

func TestTooOld(t *testing.T) {
	tests := []struct {
		version    string
		old, known bool
	}{
		{"2.29.0", false, true},
		{"2.29", false, true},
		{"2.28.0", true, true},
		{"2.9.5", true, true},    // compared as numbers...
		{"2.100.0", false, true}, // ...not as strings
		{"1.99.0", true, true},
		{"3.0.0", false, true},
		{"2.39.5 (Apple Git-154)", false, true},
		{"2.24.3 (Apple Git-128)", true, true},
		{"2.47.1.windows.1", false, true},
		{"2.50.0.rc1", false, true},
		{"", false, false},
		{"unknown", false, false},
		{"2", false, false},
		{"2.x", false, false},
		{"v2.29.0", false, false},
		{"99999999999999999999.1", false, false},
	}
	for _, tt := range tests {
		if old, known := TooOld(tt.version); old != tt.old || known != tt.known {
			t.Errorf("TooOld(%q) = %v, %v; want %v, %v", tt.version, old, known, tt.old, tt.known)
		}
	}
}

// TestEnvironment checks what a command actually receives: the gitcmd
// settings override the base environment, which otherwise passes through.
func TestEnvironment(t *testing.T) {
	fake := shtest.Script(t, t.TempDir(), "git", "env\n")
	git := &Runner{Path: fake, Env: testEnv(t,
		"GIT_TERMINAL_PROMPT=1",
		"GIT_ASKPASS=/usr/bin/ksshaskpass",
		"LC_ALL=fr_FR.UTF-8",
		"HTTPS_PROXY=http://proxy.example:3128",
	)}
	out, err := git.Run(t.Context(), "", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(out)), "\n")
	// The base values must be replaced, not just followed by gitcmd's.
	keys := make(map[string]bool)
	for _, kv := range got {
		k, _, _ := strings.Cut(kv, "=")
		if keys[k] {
			t.Errorf("%s appears twice in the environment", k)
		}
		keys[k] = true
	}
	for _, want := range []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS_REQUIRE=never",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
		"HTTPS_PROXY=http://proxy.example:3128",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("environment lacks %s:\n%s", want, out)
		}
	}
	if v := lookupEnv(got, "GIT_SSH_COMMAND"); v != "" {
		t.Errorf("GIT_SSH_COMMAND = %q without an ssh key, want it unset", v)
	}
}

func TestWithEnv(t *testing.T) {
	fake := shtest.Script(t, t.TempDir(), "git", "env\n")
	git := &Runner{Path: fake, Env: testEnv(t, "A=base", "B=base")}
	out, err := git.WithEnv("B=added", "C=added").Run(t.Context(), "", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, want := range []string{"A=base", "B=added", "C=added"} {
		if !slices.Contains(got, want) {
			t.Errorf("environment lacks %s:\n%s", want, out)
		}
	}
	if slices.Contains(got, "B=base") {
		t.Errorf("B=base wasn't replaced:\n%s", out)
	}
	if lookupEnv(git.Env, "B") != "base" || lookupEnv(git.Env, "C") != "" {
		t.Errorf("the original Runner changed: %q", git.Env)
	}
	// nil is the process environment, which the copy must keep.
	if env := (&Runner{}).WithEnv("C=added").Env; !slices.Equal(env, append(os.Environ(), "C=added")) {
		t.Errorf("from a nil Env: %q", env)
	}
}

// TestSSHKey runs the GIT_SSH_COMMAND that git builds through sh, with a fake
// ssh that records its arguments, to check the key path arrives intact.
func TestSSHKey(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	// Named ssh, so git treats it as OpenSSH.
	fakeSSH := shtest.Script(t, dir, "ssh", `printf '%s\n' "$@" > "$ARGS_FILE"; exit 1`+"\n")
	key := filepath.Join(dir, "it's my key")

	git := &Runner{
		Env:    testEnv(t, "ARGS_FILE="+argsFile, "GIT_SSH_COMMAND="+shellQuote(fakeSSH)+" -o User=tests"),
		SSHKey: key,
	}
	if _, err := git.Run(t.Context(), "", "ls-remote", "--", "ssh://git.invalid/repo.git"); err == nil {
		t.Fatal("ls-remote succeeded with a fake ssh")
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the fake ssh didn't run: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"-o", "User=tests", "-i", key, "-o", "IdentitiesOnly=yes"}
	if len(args) < len(want) || !slices.Equal(args[:len(want)], want) {
		t.Errorf("ssh args = %q, want them to start with %q", args, want)
	}
}

// TestNoPrompt checks that a server asking for credentials makes git fail at
// once: no terminal prompt, and no askpass program even when one is configured.
func TestNoPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	dir := t.TempDir()
	marker := filepath.Join(dir, "askpass-ran")
	askpass := shtest.Script(t, dir, "askpass", "touch "+shellQuote(marker)+"\necho secret\n")
	git := &Runner{Env: testEnv(t, "SSH_ASKPASS="+askpass, "DISPLAY=:0")}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := git.Run(ctx, "", "ls-remote", "--", srv.URL+"/repo.git")
	if err == nil {
		t.Fatal("ls-remote succeeded against a server that wants credentials")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("git took %v to fail, want it to fail at once", elapsed)
	}
	if !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Errorf("err = %v, want git's \"terminal prompts disabled\"", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("git ran the askpass program")
	}
}

// TestTimeout checks that a server that never answers is cut off by the context.
func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until git goes away
	}))
	defer srv.Close()

	git := &Runner{Env: testEnv(t), WaitDelay: time.Second}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := git.Run(ctx, "", "ls-remote", "--", srv.URL+"/repo.git")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run returned after %v, want about 1s", elapsed)
	}
}

// A stop ends git and every helper at once. Off Windows, git also removes its
// lock file, or the clone it had started, which it does only when asked to
// stop rather than killed. Git for Windows from 2.56 doesn't.
func TestStopCleansUp(t *testing.T) {
	requested, once := make(chan struct{}), sync.Once{}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requested) })
		<-r.Context().Done() // hang until git goes away
	}))
	defer srv.Close()

	dir := t.TempDir()
	repo, hooks, clone := filepath.Join(dir, "repo"), filepath.Join(dir, "hooks"), filepath.Join(dir, "clone")
	git := &Runner{Env: testEnv(t), WaitDelay: 10 * time.Second}
	// Loose refs, whose lock files the test waits for, whatever git's default.
	// Older git ignores the setting.
	for _, args := range [][]string{{"-c", "init.defaultRefFormat=files", "init", "-q", repo}, {"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t.invalid", "commit", "-q", "--allow-empty", "-m", "one"}} {
		if _, err := git.Run(t.Context(), "", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	// It runs while git holds the ref's lock.
	if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte("#!/bin/sh\n[ \"$1\" = prepared ] && sleep 60\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo, ".git", "refs", "heads", "x.lock")

	for _, tt := range []struct {
		name, left string
		args       []string
		started    func() bool
	}{
		{"ref lock", lock, []string{"-C", repo, "-c", "core.hooksPath=" + filepath.ToSlash(hooks), "update-ref", "refs/heads/x", "HEAD"}, func() bool {
			_, err := os.Stat(lock)
			return err == nil
		}},
		{"clone", clone, []string{"clone", "-q", "--mirror", "--", srv.URL + "/repo.git", clone}, func() bool {
			select {
			case <-requested:
				return true
			default:
				return false
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := make(chan bool, 1)
			go func() {
				for deadline := time.Now().Add(30 * time.Second); !tt.started() && time.Now().Before(deadline); {
					time.Sleep(10 * time.Millisecond)
				}
				started <- tt.started()
				// A helper that starts after the stop misses it, and lives until
				// the wait delay: let the ones git is starting run first.
				time.Sleep(500 * time.Millisecond)
				cancel()
			}()
			start := time.Now()
			_, err := git.Run(ctx, "", tt.args...)
			if !<-started {
				t.Fatalf("git never got there: %v", err)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("Run returned after %v: git or a helper didn't stop when asked, and was killed after the wait delay", elapsed)
			}
			if runtime.GOOS != "windows" {
				if _, err := os.Stat(tt.left); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s is left behind: %v", tt.left, err)
				}
			}
		})
	}
	// A helper still running would hold its request open, and Close would wait.
	closed := make(chan struct{})
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Error("a helper still holds a connection to the server")
		srv.CloseClientConnections() // so that Close returns
	}
}

func TestTailWriter(t *testing.T) {
	tests := []struct {
		name   string
		max    int
		writes []string
		want   string
	}{
		{"empty", 20, nil, ""},
		{"short", 20, []string{"fatal: one\n"}, "fatal: one"},
		{"several writes", 30, []string{"warning: a\n", "fatal: b\n\n"}, "warning: a\nfatal: b"},
		{"cut at a line", 20, []string{"0123456789\nabc\n", "fatal: x\n"}, "...\nabc\nfatal: x"},
		{"cut inside the only line", 10, []string{"0123456789abcdefghij"}, "...abcdefghij"},
		{"cut inside the last line", 10, []string{"0123\n456789abcdef\n"}, "...789abcdef"},
		{"cut inside a character", 2, []string{"abé"}, "...é"},
		{"cut leaving half a character", 1, []string{"abé"}, "..."}, // only é's last byte is kept, and dropped
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &tailWriter{max: tt.max}
			for _, s := range tt.writes {
				if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", s, n, err)
				}
			}
			if got := w.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSSHCommand(t *testing.T) {
	tests := []struct {
		base, key, want string
	}{
		{"ssh", "/keys/id_ed25519", `ssh -i '/keys/id_ed25519' -o IdentitiesOnly=yes`},
		{"ssh", "/my keys/id", `ssh -i '/my keys/id' -o IdentitiesOnly=yes`},
		{"ssh", "/keys/it's", `ssh -i '/keys/it'\''s' -o IdentitiesOnly=yes`},
		{"ssh -F /etc/backup/ssh_config", "/k", `ssh -F /etc/backup/ssh_config -i '/k' -o IdentitiesOnly=yes`},
	}
	for _, tt := range tests {
		if got := sshCommand(tt.base, tt.key); got != tt.want {
			t.Errorf("sshCommand(%q, %q) = %q, want %q", tt.base, tt.key, got, tt.want)
		}
	}
}
