// Package osprocess owns the native lifecycle of one child process, including
// its optional terminal and operating-system process tree.
package osprocess

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
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
	output     *outputCopy
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
		// Copy output here, not in os/exec. Its WaitDelay closes the pipe a
		// fixed time after exit, even while a slow writer applies
		// backpressure, and that drops the output still in the pipe.
		reader, writer, err := os.Pipe()
		if err != nil {
			_ = input.Close()
			return nil, fmt.Errorf("open process output: %w", err)
		}
		command.Stdout = writer
		command.Stderr = writer
		err = command.Start()
		// Only the child keeps the write end, so the pipe ends when it does.
		_ = writer.Close()
		if err != nil {
			_ = reader.Close()
			_ = input.Close()
			return nil, err
		}
		process.output = startOutputCopy(reader, options.Output)
	}

	tree, err := claimChild(command)
	if err != nil {
		_ = command.Process.Kill()
		_ = waitCommand(command, process.terminal)
		process.finishOutput()
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

// Wait reaps the child, reads its output to the end, and releases all native
// process resources.
func (process *Process) Wait() error {
	err := waitCommand(process.command, process.terminal)
	if expired := process.finishOutput(); expired && err == nil {
		err = exec.ErrWaitDelay
	}
	process.tree.close()
	process.finishIO()
	return err
}

// ProcessState returns the state populated by Wait.
func (process *Process) ProcessState() *os.ProcessState { return process.command.ProcessState }

// finishOutput waits for the output copy after the child exits. It reports
// whether WaitDelay closed the pipe before end of file.
func (process *Process) finishOutput() bool {
	if process.output == nil {
		return false
	}
	return process.output.wait(process.command.WaitDelay)
}

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

// outputCopy copies a child's output pipe to a writer. After the child exits,
// WaitDelay bounds only the time the copy waits on the pipe, not the time it
// waits on the writer. A descendant that keeps the pipe open cannot hold Wait
// forever, and a slow writer that applies backpressure still gets every byte.
type outputCopy struct {
	reader *os.File
	done   chan struct{}

	mu           sync.Mutex
	writing      bool
	writeStarted time.Time
	writeTime    time.Duration
}

func startOutputCopy(reader *os.File, output io.Writer) *outputCopy {
	outputCopy := &outputCopy{
		reader: reader,
		done:   make(chan struct{}),
	}
	go outputCopy.run(output)
	return outputCopy
}

func (outputCopy *outputCopy) run(output io.Writer) {
	defer close(outputCopy.done)
	// A closed read end makes further writes by the child fail instead of
	// block, as os/exec does when its copy stops.
	defer func() { _ = outputCopy.reader.Close() }()
	buffer := make([]byte, 32<<10)
	for {
		count, err := outputCopy.reader.Read(buffer)
		if count > 0 {
			outputCopy.setWriting(true)
			_, writeErr := output.Write(buffer[:count])
			outputCopy.setWriting(false)
			if writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (outputCopy *outputCopy) setWriting(writing bool) {
	outputCopy.mu.Lock()
	defer outputCopy.mu.Unlock()
	now := time.Now()
	if !writing && outputCopy.writing {
		outputCopy.writeTime += now.Sub(outputCopy.writeStarted)
	}
	outputCopy.writing = writing
	outputCopy.writeStarted = now
}

// timeInWrite returns the total time spent in the writer, including a write
// in progress.
func (outputCopy *outputCopy) timeInWrite() time.Duration {
	outputCopy.mu.Lock()
	defer outputCopy.mu.Unlock()
	total := outputCopy.writeTime
	if outputCopy.writing {
		total += time.Since(outputCopy.writeStarted)
	}
	return total
}

// wait returns after the copy reaches end of file. With a positive delay, it
// closes the pipe once the copy has waited on the pipe, not on the writer, for
// that long, and reports true. Data in the pipe is lost only then.
func (outputCopy *outputCopy) wait(delay time.Duration) bool {
	if delay <= 0 {
		<-outputCopy.done
		return false
	}
	started, writeTimeBefore := time.Now(), outputCopy.timeInWrite()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-outputCopy.done:
			return false
		case <-timer.C:
		}
		waitedOnPipe := time.Since(started) - (outputCopy.timeInWrite() - writeTimeBefore)
		if waitedOnPipe >= delay {
			err := outputCopy.reader.Close()
			<-outputCopy.done
			// The copy can reach end of file and close the pipe first.
			return !errors.Is(err, os.ErrClosed)
		}
		timer.Reset(delay - waitedOnPipe)
	}
}
