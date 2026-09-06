//go:build !windows

package osprocess

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

type processTree struct{ pid int }

func invalidExecutable(err error) bool { return errors.Is(err, syscall.ENOEXEC) }

type unixTerminal struct{ *os.File }

func (terminal unixTerminal) Resize(columns, rows int) error {
	return pty.Setsize(terminal.File, &pty.Winsize{Rows: uint16(rows), Cols: uint16(columns)})
}

func prepareChild(command *exec.Cmd, tty bool) {
	if !tty {
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
}

func startTerminal(command *exec.Cmd) (terminal, error) {
	file, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, err
	}
	return unixTerminal{file}, nil
}

func claimChild(command *exec.Cmd) (*processTree, error) {
	return &processTree{pid: command.Process.Pid}, nil
}

func (tree *processTree) stop(force bool) error {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	err := syscall.Kill(-tree.pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (*processTree) close() {}

func waitCommand(command *exec.Cmd, _ terminal) error { return command.Wait() }

func finishTerminal(processTerminal terminal, outputDone <-chan struct{}) {
	if outputDone != nil {
		<-outputDone
	}
	_ = processTerminal.Close()
}
