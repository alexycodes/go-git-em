package shtest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gitSh finds Git for Windows' sh, three levels up from git's exec path
// (mingw64/libexec/git-core). Its bin/sh.exe puts Git's tools on PATH.
var gitSh = sync.OnceValues(func() (string, error) {
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return "", err
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(strings.TrimSpace(string(out)))))
	sh := filepath.Join(root, "bin", "sh.exe")
	if _, err := os.Stat(sh); err != nil {
		return "", err
	}
	return sh, nil
})

func interpreter(t testing.TB) string {
	t.Helper()
	sh, err := gitSh()
	if err != nil {
		t.Fatalf("the tests need Git for Windows' sh: %v", err)
	}
	return filepath.ToSlash(sh)
}

// program links the test binary as script.exe, or copies it there from
// another drive. Run as that, it runs script: see init.
func program(t testing.TB, script string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe := script + ".exe"
	if os.Link(self, exe) != nil {
		if err := copyFile(self, exe); err != nil {
			t.Fatal(err)
		}
	}
	return exe
}

func copyFile(from, to string) error {
	r, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	w, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// init runs the script next to the executable, when the test binary runs as a
// program that program made. A test binary has no such script.
func init() {
	exe, err := os.Executable()
	if err != nil || !strings.EqualFold(filepath.Ext(exe), ".exe") {
		return
	}
	script := strings.TrimSuffix(exe, filepath.Ext(exe))
	sh, ok := shebang(script)
	if !ok {
		return
	}
	cmd := exec.Command(sh, append([]string{filepath.ToSlash(script)}, os.Args[1:]...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		os.Exit(exitErr.ExitCode())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "shtest:", err)
		os.Exit(127)
	}
	os.Exit(0)
}

// shebang returns the interpreter that the script at path names.
func shebang(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false
	}
	sh, ok := strings.CutPrefix(strings.TrimSpace(line), "#!")
	return sh, ok && sh != ""
}
