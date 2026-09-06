// Package osprocess owns the native lifecycle of one child process, including
// its optional terminal and operating-system process tree.
package osprocess

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

type terminal interface {
	io.ReadWriteCloser
	Resize(columns, rows int) error
}

// Options controls native process startup.
type Options struct {
	TTY    bool
	Output io.Writer
}

// Process owns a started command, its input, terminal, and process tree.
type Process struct {
	command    *exec.Cmd
	input      io.WriteCloser
	terminal   terminal
	tree       *processTree
	outputDone <-chan struct{}
}

// Start launches and claims a complete native process tree. A successful call
// guarantees that descendants are owned before the initial process can run.
func Start(command *exec.Cmd, options Options) (*Process, error) {
	if options.Output == nil {
		options.Output = io.Discard
	}
	prepareChild(command, options.TTY)
	process := &Process{command: command}
	if options.TTY {
		processTerminal, err := startTerminal(command)
		if err != nil {
			return nil, err
		}
		process.terminal = processTerminal
		process.input = processTerminal
		outputDone := make(chan struct{})
		process.outputDone = outputDone
		go func() {
			defer close(outputDone)
			_, _ = io.Copy(options.Output, processTerminal)
		}()
	} else {
		input, err := command.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("open process stdin: %w", err)
		}
		process.input = input
		command.Stdout = options.Output
		command.Stderr = options.Output
		if err := command.Start(); err != nil {
			_ = input.Close()
			return nil, err
		}
	}

	tree, err := claimChild(command)
	if err != nil {
		_ = command.Process.Kill()
		_ = waitCommand(command, process.terminal)
		process.finishIO()
		return nil, fmt.Errorf("own process tree: %w", err)
	}
	process.tree = tree
	return process, nil
}

// Input returns the child's stdin or PTY input.
func (process *Process) Input() io.WriteCloser { return process.input }

// PID returns the initial child process identifier.
func (process *Process) PID() int { return process.command.Process.Pid }

// Terminal reports whether the process owns a resizable terminal.
func (process *Process) Terminal() bool { return process.terminal != nil }

// Resize changes the terminal size.
func (process *Process) Resize(columns, rows int) error {
	if process.terminal == nil {
		return fmt.Errorf("process does not own a PTY")
	}
	return process.terminal.Resize(columns, rows)
}

// Stop requests graceful or forced termination of the whole process tree.
func (process *Process) Stop(force bool) error { return process.tree.stop(force) }

// Wait reaps the child and releases all native process resources.
func (process *Process) Wait() error {
	err := waitCommand(process.command, process.terminal)
	process.tree.close()
	process.finishIO()
	return err
}

// ProcessState returns the state populated by Wait.
func (process *Process) ProcessState() *os.ProcessState { return process.command.ProcessState }

func (process *Process) finishIO() {
	if process.terminal != nil {
		finishTerminal(process.terminal, process.outputDone)
		return
	}
	if process.input != nil {
		_ = process.input.Close()
	}
}

// InvalidExecutable reports the native error for an unexecutable image.
func InvalidExecutable(err error) bool { return invalidExecutable(err) }
