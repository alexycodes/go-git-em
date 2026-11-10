package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/alexycodes/go-git-em/internal/gitcmd"
)

// scriptCmds are the custom commands of the scripts.
var scriptCmds = map[string]func(*testscript.TestScript, bool, []string){
	"exits":         cmdExits,
	"expandenv":     cmdExpandenv,
	"snapshots":     cmdSnapshots,
	"restore":       cmdRestore,
	"refs-equal":    cmdRefsEqual,
	"bundle-has":    cmdBundleHas,
	"shares-files":  cmdSharesFiles,
	"waitsnapshots": cmdWaitSnapshots,
	"waitfor":       cmdWaitFor,
	"sigterm":       cmdSigterm,
}

// cmdExpandenv expands $VARS in a file, in place, for files that need $WORK:
//
//	expandenv <file>
func cmdExpandenv(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: expandenv <file>")
	}
	path := ts.MkAbs(args[0])
	data, err := os.ReadFile(path)
	ts.Check(err)
	ts.Check(os.WriteFile(path, []byte(os.Expand(string(data), ts.Getenv)), 0o644))
}

// cmdSnapshots checks how many snapshots an output directory holds:
//
//	snapshots <output> <n>
func cmdSnapshots(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 2 {
		ts.Fatalf("usage: snapshots <output> <n>")
	}
	want, err := strconv.Atoi(args[1])
	ts.Check(err)
	if got := snapshotsIn(ts, args[0]); len(got) != want {
		ts.Fatalf("%s has %d snapshots, want %d", args[0], len(got), want)
	}
}

// cmdWaitSnapshots waits until an output directory holds at least n
// snapshots, for a daemon running in the background:
//
//	waitsnapshots <output> <n> <timeout>
func cmdWaitSnapshots(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 3 {
		ts.Fatalf("usage: waitsnapshots <output> <n> <timeout>")
	}
	want, err := strconv.Atoi(args[1])
	ts.Check(err)
	timeout, err := time.ParseDuration(args[2])
	ts.Check(err)
	got := 0
	waitUntil(ts, timeout, func() bool { got = len(snapshotsIn(ts, args[0])); return got >= want }, func() string {
		return fmt.Sprintf("%s has %d snapshots, want at least %d", args[0], got, want)
	})
}

// cmdWaitFor waits until a file holds a match for a regexp, such as a request
// in servers.log:
//
//	waitfor <file> <timeout> <regexp>
func cmdWaitFor(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 3 {
		ts.Fatalf("usage: waitfor <file> <timeout> <regexp>")
	}
	timeout, err := time.ParseDuration(args[1])
	ts.Check(err)
	re, err := regexp.Compile(args[2])
	ts.Check(err)
	path := ts.MkAbs(args[0])
	waitUntil(ts, timeout, func() bool { data, err := os.ReadFile(path); return err == nil && re.Match(data) }, func() string {
		return fmt.Sprintf("%s has no match for %q", args[0], args[2])
	})
}

// waitUntil polls cond for at most timeout, and fails the script with
// failure() if it never holds. It gives up as soon as a go-git-em running in
// the background exits, so a daemon that dies at startup doesn't cost the
// whole timeout.
func waitUntil(ts *testscript.TestScript, timeout time.Duration, cond func() bool, failure func() string) {
	var held, gone bool
	poll(timeout, func() bool {
		// The exit first, then cond: a command that meets cond and then exits
		// still passes.
		gone = exited(ts)
		held = cond()
		return held || gone
	})
	switch {
	case held:
	case gone:
		ts.Fatalf("go-git-em, running in the background, has exited: %s", failure())
	default:
		ts.Fatalf("after %v: %s", timeout, failure())
	}
}

// exited reports whether a go-git-em running in the background has exited.
// Signal 0 sends nothing, and fails once testscript has waited for it.
func exited(ts *testscript.TestScript) bool {
	for _, cmd := range ts.BackgroundCmds() {
		if filepath.Base(cmd.Args[0]) == "go-git-em" && errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone) {
			return true
		}
	}
	return false
}

// cmdSigterm sends SIGTERM, as docker stop and systemd do, to the commands
// running in the background. testscript's kill sends only SIGINT or SIGKILL,
// and a process may have inherited SIGINT as ignored, as a background job does:
// go-git-em then keeps ignoring it.
//
//	sigterm
func cmdSigterm(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 0 {
		ts.Fatalf("usage: sigterm")
	}
	signalled := 0
	for _, cmd := range ts.BackgroundCmds() {
		err := cmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			continue // it has exited, though wait hasn't run yet
		}
		ts.Check(err)
		signalled++
	}
	if signalled == 0 {
		ts.Fatalf("no command is running in the background")
	}
}

// poll checks cond every 10 ms until it holds, for at most timeout.
func poll(timeout time.Duration, cond func() bool) {
	for deadline := time.Now().Add(timeout); !cond() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
}

// cmdRestore clones a snapshot with plain git, then checks the clone with
// fsck --strict. A snapshot is "latest" or its number, oldest first from 1.
//
//	restore <output> <n|latest> <dir>
func cmdRestore(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 3 {
		ts.Fatalf("usage: restore <output> <n|latest> <dir>")
	}
	dir := ts.MkAbs(args[2])
	scriptGit(ts, ts.MkAbs("."), "clone", "--quiet", snapshotAt(ts, args[0], args[1]), dir)
	scriptGit(ts, dir, "fsck", "--strict", "--no-progress")
}

// cmdRefsEqual checks that a repository's refs, and its HEAD, are those of a
// snapshot:
//
//	refs-equal <repo> <output> <n|latest>
func cmdRefsEqual(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 3 {
		ts.Fatalf("usage: refs-equal <repo> <output> <n|latest>")
	}
	out, err := gitIn(ts, ts.MkAbs(args[0]), "show-ref", "--head")
	if err != nil && gitcmd.ExitCode(err) != 1 { // 1: no refs
		ts.Fatalf("%v", err)
	}
	repo := sortedLines(out)
	bundle := snapshotAt(ts, args[1], args[2])
	snapshot := sortedLines(scriptGit(ts, filepath.Dir(bundle), "bundle", "list-heads", bundle))
	if slices.Equal(repo, snapshot) == neg {
		ts.Fatalf("%s's refs:\n%s\nsnapshot %s's refs:\n%s", args[0], strings.Join(repo, "\n"), args[2], strings.Join(snapshot, "\n"))
	}
}

// cmdBundleHas checks that a snapshot holds an object: anything rev-parse
// takes, such as a ref, a commit id or ":/<message>".
//
//	bundle-has <output> <n|latest> <object>
func cmdBundleHas(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 3 {
		ts.Fatalf("usage: bundle-has <output> <n|latest> <object>")
	}
	clone, err := os.MkdirTemp(ts.Getenv("WORK"), "bundle-has-") // removed with $WORK
	ts.Check(err)
	scriptGit(ts, clone, "clone", "--quiet", "--mirror", snapshotAt(ts, args[0], args[1]), ".")
	id, err := gitIn(ts, clone, "rev-parse", "--verify", "--quiet", args[2])
	if err == nil { // rev-parse takes a full object id without looking it up
		_, err = gitIn(ts, clone, "rev-parse", "--verify", "--quiet", strings.TrimSpace(id)+"^{object}")
	}
	if err != nil && gitcmd.ExitCode(err) != 1 { // 1: not found
		ts.Fatalf("%v", err)
	}
	if (err == nil) == neg {
		ts.Fatalf("snapshot %s of %s holds %s: %v", args[1], args[0], args[2], err == nil)
	}
}

// cmdSharesFiles checks whether two directories share a file, through a hard
// link:
//
//	shares-files <dir1> <dir2>
func cmdSharesFiles(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 {
		ts.Fatalf("usage: shares-files <dir1> <dir2>")
	}
	a, b := files(ts, args[0]), files(ts, args[1])
	shared := slices.ContainsFunc(a, func(fa fs.FileInfo) bool {
		return slices.ContainsFunc(b, func(fb fs.FileInfo) bool { return os.SameFile(fa, fb) })
	})
	if shared == neg {
		ts.Fatalf("%s and %s share a file: %v", args[0], args[1], shared)
	}
}

func files(ts *testscript.TestScript, dir string) []fs.FileInfo {
	var infos []fs.FileInfo
	ts.Check(filepath.WalkDir(ts.MkAbs(dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		infos = append(infos, info)
		return err
	}))
	return infos
}

var snapshotFile = regexp.MustCompile(`^\d{8}T\d{6}\.\d{3}Z\.bundle$`)

// snapshotsIn returns the snapshot files of an output directory, oldest first.
func snapshotsIn(ts *testscript.TestScript, output string) []string {
	dir := filepath.Join(ts.MkAbs(output), "snapshots")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	ts.Check(err)
	var paths []string
	for _, e := range entries {
		if snapshotFile.MatchString(e.Name()) {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths
}

// snapshotAt returns a snapshot's path: "latest", or its number, oldest first
// from 1.
func snapshotAt(ts *testscript.TestScript, output, which string) string {
	paths := snapshotsIn(ts, output)
	i := len(paths)
	if which != "latest" {
		var err error
		i, err = strconv.Atoi(which)
		ts.Check(err)
	}
	if i < 1 || i > len(paths) {
		ts.Fatalf("%s has %d snapshots, so no snapshot %s", output, len(paths), which)
	}
	return paths[i-1]
}

// scriptGit runs git in dir as the script would, and fails the script if git
// fails.
func scriptGit(ts *testscript.TestScript, dir string, args ...string) string {
	out, err := gitIn(ts, dir, args...)
	ts.Check(err)
	return out
}

// gitIn runs git in dir with the script's environment.
func gitIn(ts *testscript.TestScript, dir string, args ...string) (string, error) {
	var env []string
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL"} {
		env = append(env, key+"="+ts.Getenv(key))
	}
	// A hung git would otherwise hold the whole test binary until go test's timeout.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := (&gitcmd.Runner{Env: env}).Run(ctx, dir, args...)
	return string(out), err
}

func sortedLines(s string) []string {
	var lines []string
	for line := range strings.Lines(s) {
		lines = append(lines, strings.TrimSuffix(line, "\n"))
	}
	slices.Sort(lines)
	return lines
}

// TestScriptCommandsFail checks that each command fails when it should, which
// the scenarios alone can't show: a check that never fails would pass them
// all. Each script must fail, with the given message.
func TestScriptCommandsFail(t *testing.T) {
	self, err := exec.LookPath("go-git-em")
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// A repository and one snapshot of it, before a second commit.
	const fixture = `exec git init -q src
exec git -C src commit -q --allow-empty -m one
mkdir out/snapshots
exec git -C src bundle create -q $WORK/out/snapshots/20260101T000000.000Z.bundle --all
`
	scripts := map[string]struct{ script, wantLog string }{
		"refs_differ":          {fixture + "exec git -C src commit -q --allow-empty -m two\nrefs-equal src out latest\n", "snapshot latest's refs"},
		"refs_equal":           {fixture + "! refs-equal src out latest\n", "snapshot latest's refs"},
		"object_missing":       {fixture + "bundle-has out latest :/no-such-commit\n", "holds :/no-such-commit: false"},
		"object_present":       {fixture + "! bundle-has out latest refs/heads/main\n", "holds refs/heads/main: true"},
		"object_id_absent":     {fixture + "bundle-has out latest 0123456789abcdef0123456789abcdef01234567\n", "holds 0123456789abcdef0123456789abcdef01234567: false"},
		"no_shared_file":       {fixture + "shares-files src out\n", "share a file: false"},
		"shared_file":          {fixture + "exec git clone -q --bare src linked.git\n! shares-files src/.git/objects linked.git/objects\n", "share a file: true"},
		"snapshot_count":       {fixture + "snapshots out 2\n", "out has 1 snapshots, want 2"},
		"no_such_snapshot":     {fixture + "restore out 2 restored\n", "out has 1 snapshots, so no snapshot 2"},
		"damaged_restore":      {"mkdir out/snapshots\ncp garbage out/snapshots/20260101T000000.000Z.bundle\nrestore out latest restored\n-- garbage --\nnot a bundle\n", "git clone"},
		"wrong_exit_code":      {"exits 0 go-git-em -no-such-flag\n", "exit code 2, want 0"},
		"too_few_snapshots":    {fixture + "waitsnapshots out 2 100ms\n", "after 100ms: out has 1 snapshots, want at least 2"},
		"exited_meanwhile":     {"exec go-git-em -version &\nwaitsnapshots out 1 1m\n", "go-git-em, running in the background, has exited: out has 0 snapshots, want at least 1"},
		"exited_meanwhile_too": {"exec go-git-em -version &\nwaitfor log 1m nothing\n-- log --\nhttp: GET /repo.git\n", `go-git-em, running in the background, has exited: log has no match for "nothing"`},
		"nothing_to_signal":    {"sigterm\n", "no command is running in the background"},
		"never_matches":        {"waitfor log 100ms 'hang: '\n-- log --\nhttp: GET /repo.git\n", `after 100ms: log has no match for "hang: "`},
		// git failing isn't "no refs", even where differing refs would pass.
		"not_a_repository": {fixture + "mkdir plain\n! refs-equal plain out latest\n", "not a git repository"},
	}
	dir := t.TempDir()
	for name, s := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name+".txtar"), []byte(s.script), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := &recordT{T: t, mu: new(sync.Mutex), failed: map[string]bool{}, logs: map[string]string{}}
	testscript.RunT(rec, testscript.Params{
		Dir:   dir,
		Setup: setup(filepath.Dir(self), map[string]string{"git": gitPath}),
		Cmds:  scriptCmds,
	})
	for name, s := range scripts {
		if !rec.failed[name] {
			t.Errorf("%s: the script passed, want it to fail", name)
		} else if !strings.Contains(rec.logs[name], s.wantLog) {
			t.Errorf("%s: failed, but not with %q:\n%s", name, s.wantLog, rec.logs[name])
		}
	}
}

// recordT is a testscript.T that records which scripts fail, and their logs,
// instead of failing the test.
type recordT struct {
	*testing.T
	name   string
	mu     *sync.Mutex
	failed map[string]bool
	logs   map[string]string
}

func (r *recordT) Run(name string, f func(testscript.T)) {
	sub := &recordT{T: r.T, name: name, mu: r.mu, failed: r.failed, logs: r.logs}
	done := make(chan struct{})
	go func() { // FailNow ends this goroutine, as testing.T's does
		defer close(done)
		f(sub)
	}()
	<-done
}

func (r *recordT) Log(args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs[r.name] += fmt.Sprintln(args...)
}

func (r *recordT) FailNow() {
	r.mu.Lock()
	r.failed[r.name] = true
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *recordT) Fatal(args ...any) {
	r.Log(args...)
	r.FailNow()
}

func (r *recordT) Skip(args ...any) {
	r.Log(args...)
	runtime.Goexit()
}

func (r *recordT) Parallel()     {}
func (r *recordT) Verbose() bool { return false }
