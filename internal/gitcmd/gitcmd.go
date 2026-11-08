// Package gitcmd runs git. It is the only package that starts programs, so every
// git command, and every helper git starts in turn (git-remote-https, ssh), runs
// with the same environment, the same isolation from the terminal and the same
// cancellation.
package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// DefaultWaitDelay is how long a cancelled command gets to exit after SIGTERM,
// and how long Run waits for output pipes that a leftover helper holds open.
const DefaultWaitDelay = 5 * time.Second

// stderrTail is how much of the end of git's stderr an Error keeps.
const stderrTail = 4 << 10

// Runner runs git commands. The zero value runs "git" from PATH with the
// process environment. A Runner is safe for concurrent use.
type Runner struct {
	// Path is the git program. Empty means "git", looked up on PATH.
	Path string
	// Env is the environment git starts from, as KEY=value pairs.
	// nil means the process environment.
	Env []string
	// SSHKey is a private key file for ssh inputs. Empty leaves the choice of
	// key to OpenSSH and the user's own configuration.
	SSHKey string
	// WaitDelay overrides DefaultWaitDelay when positive.
	WaitDelay time.Duration
}

// Run runs git with args in dir ("" means the current directory) and returns
// its standard output. When ctx is done, git and its helpers get SIGTERM, then
// SIGKILL after the wait delay. If git fails or can't start, the error is an
// *Error, and the output is whatever git wrote before that.
func (r *Runner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	path := r.Path
	if path == "" {
		path = "git"
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = r.environ()
	// Stdin stays nil, which connects it to the null device: nothing can be typed in.
	var stdout bytes.Buffer
	stderr := &tailWriter{max: stderrTail}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = DefaultWaitDelay
	if r.WaitDelay > 0 {
		cmd.WaitDelay = r.WaitDelay
	}
	isolate(cmd)

	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
		// Helpers that outlived git, such as one that ignored SIGTERM.
		killGroup(cmd)
	}
	if err != nil {
		if ctx.Err() != nil {
			// Report the timeout or cancellation, not the signal that ended git.
			err = ctx.Err()
		}
		return stdout.Bytes(), &Error{Args: args, Err: err, Stderr: stderr.String()}
	}
	return stdout.Bytes(), nil
}

// Version returns git's version, such as "2.47.3" or "2.39.5 (Apple Git-154)".
func (r *Runner) Version(ctx context.Context) (string, error) {
	out, err := r.Run(ctx, "", "version")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "git version "), nil
}

// environ returns git's environment: the base environment plus the settings
// that keep git and its helpers from ever waiting for input. Later entries
// override earlier ones with the same key.
func (r *Runner) environ() []string {
	base := r.Env
	if base == nil {
		base = os.Environ()
	}
	env := append(slices.Clip(base),
		// No username or password prompt on the terminal...
		"GIT_TERMINAL_PROMPT=0",
		// ...and no askpass program either. An empty GIT_ASKPASS also stops git
		// from falling back to core.askPass and SSH_ASKPASS.
		"GIT_ASKPASS=",
		// OpenSSH would otherwise start SSH_ASKPASS when DISPLAY is set, since
		// it has no terminal to prompt on.
		"SSH_ASKPASS_REQUIRE=never",
		// Git Credential Manager: no windows.
		"GCM_INTERACTIVE=never",
		// git's messages in the logs are always in English.
		"LC_ALL=C",
	)
	if r.SSHKey != "" {
		sshBase := lookupEnv(base, "GIT_SSH_COMMAND")
		if sshBase == "" {
			sshBase = "ssh"
		}
		env = append(env, "GIT_SSH_COMMAND="+sshCommand(sshBase, r.SSHKey))
	}
	return env
}

// lookupEnv returns the last value of key in env, or "".
func lookupEnv(env []string, key string) string {
	for _, kv := range slices.Backward(env) {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// Error is a git command that failed, or couldn't start.
type Error struct {
	Args   []string // git's arguments
	Err    error    // the exit status, the context's error, or why git couldn't start
	Stderr string   // the end of git's stderr
}

func (e *Error) Error() string {
	msg := "git " + strings.Join(e.Args, " ") + ": " + e.Err.Error()
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode returns git's exit code, or -1 if there is none: git couldn't
// start, was killed by a signal, or was stopped because the context ended.
func (e *Error) ExitCode() int {
	var exitErr *exec.ExitError
	if errors.As(e.Err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// ExitCode returns the exit code of the git command err is about: 0 for nil,
// and -1 for an error that isn't an *Error.
func ExitCode(err error) int {
	var gitErr *Error
	switch {
	case err == nil:
		return 0
	case errors.As(err, &gitErr):
		return gitErr.ExitCode()
	}
	return -1
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	max       int
	buf       []byte
	truncated bool
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if over := len(w.buf) - w.max; over > 0 {
		w.buf = slices.Clone(w.buf[over:])
		w.truncated = true
	}
	return len(p), nil
}

// String returns the kept text without trailing white space. If the start
// was cut off, it begins with "...". The partial first line is dropped,
// unless it's the only line: that's the end of git's last message.
func (w *tailWriter) String() string {
	s := strings.TrimRight(string(w.buf), " \t\r\n")
	if !w.truncated {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return "...\n" + s[i+1:]
	}
	// The cut may have split a multi-byte character.
	return "..." + strings.ToValidUTF8(s, "")
}
