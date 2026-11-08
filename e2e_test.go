package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// The programs go-git-em may run. They are the only programs on a script's PATH,
// besides go-git-em itself.
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

// TestMain lets the test binary run as the go-git-em command inside scripts.
func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"go-git-em": main,
	})
}

func TestScripts(t *testing.T) {
	// testscript.Main put the directory holding the go-git-em command first on PATH.
	self, err := exec.LookPath("go-git-em")
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
		Cmds:                map[string]func(*testscript.TestScript, bool, []string){"exits": cmdExits},
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

// setup gives each script a hermetic git setup, and a PATH that holds only
// go-git-em ($CMDBIN) and the declared programs ($WORK/tools).
func setup(cmdBin string, tools map[string]string) func(*testscript.Env) error {
	return func(env *testscript.Env) error {
		toolsDir := filepath.Join(env.WorkDir, "tools")
		home := filepath.Join(env.WorkDir, "home")
		gitconfig := filepath.Join(env.WorkDir, ".gitconfig")

		for _, dir := range []string{toolsDir, home} {
			if err := os.Mkdir(dir, 0o755); err != nil {
				return err
			}
		}
		for name, path := range tools {
			if err := os.Symlink(path, filepath.Join(toolsDir, name)); err != nil {
				return err
			}
		}
		if err := os.WriteFile(gitconfig, []byte(testGitconfig), 0o644); err != nil {
			return err
		}

		env.Setenv("PATH", cmdBin+string(os.PathListSeparator)+toolsDir)
		env.Setenv("CMDBIN", cmdBin)
		env.Setenv("HOME", home)
		env.Setenv("GIT_CONFIG_NOSYSTEM", "1")
		env.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
		return nil
	}
}
