//go:build windows

package osprocess

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"github.com/charmbracelet/x/conpty"
	"golang.org/x/sys/windows"
)

func invalidExecutable(err error) bool { return errors.Is(err, windows.ERROR_BAD_EXE_FORMAT) }

type processTree struct {
	mu  sync.Mutex
	job windows.Handle
	pid uint32
}

func prepareChild(command *exec.Cmd, _ bool) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP}
}

func startTerminal(command *exec.Cmd) (terminal, error) {
	processTerminal, err := conpty.New(80, 24, 0)
	if err != nil {
		return nil, err
	}
	pid, handle, err := processTerminal.Spawn(command.Path, command.Args, &syscall.ProcAttr{Dir: command.Dir, Env: command.Env, Sys: command.SysProcAttr})
	if err != nil {
		_ = processTerminal.Close()
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(windows.Handle(handle)) }()
	command.Process, err = os.FindProcess(pid)
	if err != nil {
		_ = windows.TerminateProcess(windows.Handle(handle), 1)
		_ = processTerminal.Close()
		return nil, err
	}
	return processTerminal, nil
}

func claimChild(command *exec.Cmd) (*processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = windows.CloseHandle(job)
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(process) }()
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return nil, fmt.Errorf("assign suspended process to job: %w", err)
	}
	if err := resumeInitialThread(uint32(command.Process.Pid)); err != nil {
		return nil, err
	}
	success = true
	return &processTree{job: job, pid: uint32(command.Process.Pid)}, nil
}

func resumeInitialThread(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		return err
	}
	return fmt.Errorf("find suspended thread for process %d: %w", pid, err)
}

func (tree *processTree) stop(force bool) error {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.job == 0 {
		return nil
	}
	if !force {
		if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, tree.pid); err == nil {
			return nil
		}
	}
	return windows.TerminateJobObject(tree.job, 1)
}

func (tree *processTree) close() {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.job != 0 {
		_ = windows.CloseHandle(tree.job)
		tree.job = 0
	}
}

func waitCommand(command *exec.Cmd, processTerminal terminal) error {
	if processTerminal == nil {
		return command.Wait()
	}
	state, err := command.Process.Wait()
	command.ProcessState = state
	if err == nil && !state.Success() {
		return &exec.ExitError{ProcessState: state}
	}
	return err
}

func finishTerminal(processTerminal terminal, outputDone <-chan struct{}) {
	_ = processTerminal.Close()
	if outputDone != nil {
		<-outputDone
	}
}
