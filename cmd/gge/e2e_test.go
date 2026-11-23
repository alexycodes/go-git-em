package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/alexycodes/go-git-em/internal/schedule"
)

// The programs go-git-em may run. They are the only programs on a script's PATH,
// besides go-git-em itself, except on Windows: see toolsPath.
var declaredTools = []string{"git", "ssh"}

// testGitconfig is the global git config of every script.
const testGitconfig = `[user]
	name = go-git-em tests
	email = tests@go-git-em.invalid
[init]
	defaultBranch = main
[protocol "file"]
	allow = always
`

// TestMain lets the test binary run as the gge command inside scripts. There,
// and in the tests, schedules may be "@every 1s", so the daemon runs in
// seconds rather than at the next minute.
func TestMain(m *testing.M) {
	schedule.AcceptEvery = true
	testscript.Main(m, map[string]func(){
		"gge": gge,
	})
}

func TestScripts(t *testing.T) {
	// testscript.Main put the directory holding the gge command first on PATH.
	self, err := exec.LookPath("gge")
	if err != nil {
		t.Fatal(err)
	}
	tools := make(map[string]string)
	for _, name := range declaredTools {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("the tests need %s on PATH: %v", name, err)
		}
		tools[name] = path
	}

	testscript.Run(t, testscript.Params{
		Dir:                 "testdata/script",
		Setup:               setup(filepath.Dir(self), tools),
		Cmds:                scriptCmds,
		RequireExplicitExec: true,
		RequireUniqueNames:  true,
	})
}

// cmdExits runs a program and checks its exit code, which exec can't:
//
//	exits <code> <program> [args...]
//
// Afterwards stdout and stderr hold the program's output, as after exec.
func cmdExits(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("unsupported: ! exits")
	}
	if len(args) < 2 {
		ts.Fatalf("usage: exits <code> <program> [args...]")
	}
	want, err := strconv.Atoi(args[0])
	ts.Check(err)
	got := 0
	if err := ts.Exec(args[1], args[2:]...); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			ts.Fatalf("%v", err)
		}
		got = exitErr.ExitCode()
	}
	if got != want {
		ts.Fatalf("exit code %d, want %d", got, want)
	}
}

// setup gives each script a hermetic git setup, a PATH that holds only
// gge ($CMDBIN) and the declared programs (see toolsPath), and servers for
// the repositories in $WORK/srv.
func setup(cmdBin string, tools map[string]string) func(*testscript.Env) error {
	return func(env *testscript.Env) error {
		home := filepath.Join(env.WorkDir, "home")
		// In HOME, where git before 2.32 looks: it ignores GIT_CONFIG_GLOBAL.
		gitconfig := filepath.Join(home, ".gitconfig")

		if err := os.Mkdir(home, 0o755); err != nil {
			return err
		}
		path, err := toolsPath(env.WorkDir, tools)
		if err != nil {
			return err
		}
		if err := os.WriteFile(gitconfig, []byte(testGitconfig), 0o644); err != nil {
			return err
		}

		env.Setenv("PATH", cmdBin+string(os.PathListSeparator)+path)
		env.Setenv("CMDBIN", cmdBin)
		env.Setenv("HOME", home)
		env.Setenv("GIT_CONFIG_NOSYSTEM", "1")
		env.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
		env.Setenv("SEP", sepRE)
		env.Setenv("WORKRE", pathRE(env.WorkDir))
		env.Setenv("HOMERE", pathRE(home))

		// The servers' git gets the same configuration. They aren't the product,
		// so their PATH is the test's.
		serverEnv := []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + gitconfig,
		}
		return startServers(env, tools["git"], serverEnv)
	}
}

// toolsPath returns the PATH for the declared programs: $WORK/tools, which
// links to them. On Windows, it's their own directories: Git for Windows
// finds its parts next to git.exe, so it can't be linked elsewhere.
func toolsPath(workDir string, tools map[string]string) (string, error) {
	var dirs []string
	if runtime.GOOS == "windows" {
		for _, name := range declaredTools {
			if path, ok := tools[name]; ok {
				dirs = append(dirs, filepath.Dir(path))
			}
		}
		return strings.Join(dirs, string(os.PathListSeparator)), nil
	}
	dir := filepath.Join(workDir, "tools")
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	for name, path := range tools {
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// sepRE matches a path separator as gge logs it: on Windows, a backslash,
// which a quoted value doubles.
var sepRE = func() string {
	if runtime.GOOS == "windows" {
		return `\\{1,2}`
	}
	return "/"
}()

// pathRE returns a regexp for path as gge logs it, as $WORKRE and $HOMERE.
func pathRE(path string) string {
	return strings.ReplaceAll(regexp.QuoteMeta(filepath.ToSlash(path)), "/", sepRE)
}
