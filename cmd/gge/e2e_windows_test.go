package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// childEnv marks the go-git-em that the gge command starts.
const childEnv = "GGE_TEST_CHILD"

// gge is the gge command in scripts. It runs go-git-em in a process group of
// its own, so that stop can send it Ctrl-Break: Windows has no SIGTERM, and
// testscript can't start a background command in a new group. A job ties
// go-git-em to the gge command, which is all that testscript can kill.
func gge() {
	if os.Getenv(childEnv) != "" {
		main()
		return
	}
	err := runChild()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		os.Exit(exitErr.ExitCode())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gge:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runChild() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	// Never closed: the job goes with this process, and go-git-em with it.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return err
	}
	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Any git that go-git-em starts before this dies with go-git-em anyway.
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, p)
		_ = windows.CloseHandle(p)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	return cmd.Wait()
}

// stop sends Ctrl-Break to the go-git-em that cmd, a gge command running in
// the background, started. Go reads it as os.Interrupt.
func stop(cmd *exec.Cmd) error {
	if !isGGE(cmd) {
		return errors.New("on Windows, only gge can be stopped")
	}
	var pid uint32
	var err error
	poll(10*time.Second, func() bool {
		pid, err = child(cmd.Process.Pid)
		return pid != 0 || err != nil || done(cmd)
	})
	switch {
	case done(cmd):
		return os.ErrProcessDone
	case err != nil:
		return err
	case pid == 0:
		return errors.New("gge hasn't started go-git-em")
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, pid)
}

// child returns the process ID of the go-git-em that the gge command with
// process ID parent started, or 0 if there's none.
func child(parent int) (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if entry.ParentProcessID == uint32(parent) && strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), "gge.exe") {
			return entry.ProcessID, nil
		}
	}
	if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return 0, nil
	}
	return 0, err
}
