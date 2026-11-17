package gitcmd

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// group is a job object holding git and every helper it starts. Git for
// Windows' git.exe is itself a launcher that starts the real git.
type group struct {
	cmd *exec.Cmd

	mu       sync.Mutex
	job      windows.Handle // 0 until started, and once killed
	stopping bool
}

// isolate starts cmd suspended, so that started can put it in the job before
// it starts anything, and in a new process group, which Ctrl-Break reaches
// alone. From Git for Windows 2.56, git leaves its lock files on Ctrl-Break:
// the next backup removes them.
func isolate(cmd *exec.Cmd) *group {
	g := &group{cmd: cmd}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED}
	cmd.Cancel = g.stop
	return g
}

// started puts git in the job and resumes it.
func (g *group) started() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	pid := uint32(g.cmd.Process.Pid)
	job, err := newJob(pid)
	if err != nil {
		return fmt.Errorf("put git in a job object: %w", err)
	}
	g.job = job
	if g.stopping { // the context ended while git was suspended
		return windows.TerminateJobObject(job, 1)
	}
	if err := resume(pid); err != nil {
		return fmt.Errorf("resume git: %w", err)
	}
	return nil
}

// stop sends Ctrl-Break to git's process group. Without a console to send it
// through, as when go-git-em was started without one, it kills the job.
func (g *group) stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopping = true
	if g.job == 0 {
		return nil // started kills it
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(g.cmd.Process.Pid)); err != nil {
		return windows.TerminateJobObject(g.job, 1)
	}
	return nil
}

// kill kills whatever is left in the job after git exited.
func (g *group) kill() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		_ = windows.TerminateJobObject(g.job, 1)
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
}

// newJob returns a job holding the process pid. The job kills its processes
// when its last handle closes, so they also die with go-git-em.
func newJob(pid uint32) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err == nil {
		err = assign(job, pid)
	}
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func assign(job windows.Handle, pid uint32) error {
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(p) }()
	return windows.AssignProcessToJobObject(job, p)
}

// resume resumes the suspended process pid. exec doesn't keep the handle to
// its thread, so it's found among the system's threads. Any other thread,
// such as one that security software started, is running already, and
// resuming it does nothing.
func resume(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	found := false
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		if err := resumeThread(entry.ThreadID); err != nil {
			return err
		}
		found = true
	}
	switch {
	case !errors.Is(err, windows.ERROR_NO_MORE_FILES):
		return err
	case !found:
		return errors.New("no thread found")
	}
	return nil
}

func resumeThread(id uint32) error {
	thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, id)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(thread) }()
	_, err = windows.ResumeThread(thread)
	return err
}
