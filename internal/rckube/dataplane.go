/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rckube

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	processruntime "github.com/nekomeowww/rc/internal/execution"
)

const dataPlaneIDPrefix = "envd_"

// DataPlaneStreamKind identifies the output stream of a Data Plane process chunk.
type DataPlaneStreamKind uint8

const (
	DataPlaneStreamStdout DataPlaneStreamKind = iota + 1
	DataPlaneStreamStderr
	DataPlaneStreamPTY
)

// DataPlaneChunk is one typed output chunk from a Data Plane process.
type DataPlaneChunk struct {
	Kind DataPlaneStreamKind
	Data []byte
}

// DataPlaneProcessConfig describes a process started through the E2B envd Data Plane.
type DataPlaneProcessConfig struct {
	Cmd   string
	Args  []string
	Envs  map[string]string
	Cwd   string
	Tag   string
	PTY   bool
	Cols  uint16
	Rows  uint16
	Stdin bool
}

// DataPlaneProcessInfo is the external snapshot of a Data Plane process.
type DataPlaneProcessInfo struct {
	ID       string
	PID      uint32
	Tag      string
	Config   DataPlaneProcessConfig
	Running  bool
	ExitCode int32
	Exited   bool
	Status   string
	Error    string
}

type dataPlaneSubscriber struct {
	chunks chan DataPlaneChunk
}

// DataPlaneSubscription receives typed output chunks from a Data Plane process
// until the process exits or Close is called.
type DataPlaneSubscription struct {
	process *supervisedProcess
	id      uint64
	sub     *dataPlaneSubscriber
	replay  []DataPlaneChunk
	exit    <-chan struct{}
	closed  bool
}

// Replay returns output chunks recorded before the subscription opened.
func (s *DataPlaneSubscription) Replay() []DataPlaneChunk {
	return s.replay
}

// Chunks returns the live output channel.
func (s *DataPlaneSubscription) Chunks() <-chan DataPlaneChunk {
	if s.sub == nil {
		return nil
	}
	return s.sub.chunks
}

// Exit closes when the supervised process finishes.
func (s *DataPlaneSubscription) Exit() <-chan struct{} {
	return s.exit
}

// Outcome returns the terminal process status after Exit has closed.
func (s *DataPlaneSubscription) Outcome() DataPlaneProcessInfo {
	return s.process.dataPlaneInfo()
}

// Close unregisters this subscriber.
func (s *DataPlaneSubscription) Close() {
	if s.closed || s.sub == nil || s.process == nil {
		return
	}
	s.closed = true
	s.process.mu.Lock()
	delete(s.process.dataPlaneSubs, s.id)
	s.process.mu.Unlock()
}

// HomeDir returns the supervisor's configured home directory.
func (supervisor *Supervisor) HomeDir() string {
	return supervisor.homeDir
}

// WorkspaceDir returns the supervisor's configured workspace directory.
func (supervisor *Supervisor) WorkspaceDir() string {
	return supervisor.workspaceDir
}

func (supervisor *Supervisor) hasRunningDataPlaneProcess() bool {
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	for _, process := range supervisor.processes {
		if !process.isDataPlane {
			continue
		}
		process.mu.Lock()
		running := process.state.Phase == phaseRunning || process.state.Phase == phaseStarting
		process.mu.Unlock()
		if running {
			return true
		}
	}
	return false
}

// StartDataPlaneProcess launches a process under the supervisor with an `envd_`
// ID prefix and returns both its snapshot and a live subscription attached
// before any output is missed.
func (supervisor *Supervisor) StartDataPlaneProcess(ctx context.Context, cfg DataPlaneProcessConfig) (DataPlaneProcessInfo, *DataPlaneSubscription, error) {
	if cfg.Cmd == "" {
		return DataPlaneProcessInfo{}, nil, errors.New("command is required")
	}
	if err := ctx.Err(); err != nil {
		return DataPlaneProcessInfo{}, nil, err
	}
	seq := supervisor.dataPlaneSeq.Add(1)
	id := fmt.Sprintf("%s%d_%d", dataPlaneIDPrefix, time.Now().UTC().UnixNano(), seq)

	command := make([]string, 0, 1+len(cfg.Args))
	command = append(command, cfg.Cmd)
	command = append(command, cfg.Args...)

	request := processruntime.StartRequest{
		ID:               id,
		UID:              id,
		Command:          command,
		WorkingDirectory: cfg.Cwd,
		DefaultDirectory: processruntime.DefaultDirectoryHome,
		Environment:      maps.Clone(cfg.Envs),
		TTY:              cfg.PTY,
		Stdin:            cfg.Stdin,
	}

	supervisor.Touch(time.Now())
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return DataPlaneProcessInfo{}, nil, err
	}

	stateDirectory := fmt.Sprintf("%s/%s", supervisor.stateDir, request.ID)
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return DataPlaneProcessInfo{}, nil, fmt.Errorf("create persistent process state directory: %w", err)
	}
	ownerPath := fmt.Sprintf("%s/owner.uid", stateDirectory)
	if err := os.WriteFile(ownerPath, []byte(request.UID), 0o600); err != nil {
		return DataPlaneProcessInfo{}, nil, fmt.Errorf("record persistent process ownership: %w", err)
	}

	cfgCopy := cfg
	cfgCopy.Args = slices.Clone(cfg.Args)
	cfgCopy.Envs = maps.Clone(cfg.Envs)

	sub := &dataPlaneSubscriber{chunks: make(chan DataPlaneChunk, 256)}
	process := &supervisedProcess{
		state:            processruntime.State{ID: request.ID, UID: request.UID, Phase: phaseStarting},
		exit:             make(chan struct{}),
		clients:          make(map[string]*attachClient),
		clientSizes:      make(map[string]terminalSize),
		transcriptMax:    supervisor.maxLog,
		supervisor:       supervisor,
		processID:        request.ID,
		command:          slices.Clone(request.Command),
		workingDirectory: request.WorkingDirectory,
		tty:              request.TTY,
		isDataPlane:      true,
		dataPlaneTag:     cfg.Tag,
		dataPlaneConfig:  &cfgCopy,
		dataPlaneSubs:    map[uint64]*dataPlaneSubscriber{1: sub},
		nextDataPlaneID:  1,
	}
	attempt := &processStart{
		supervisor: supervisor,
		ownerPath:  ownerPath,
		process:    process,
	}
	if _, err := attempt.runLocked(ctx, request); err != nil {
		return DataPlaneProcessInfo{}, nil, err
	}
	if cfg.PTY && cfg.Cols > 0 && cfg.Rows > 0 && process.terminal != nil {
		_ = process.terminal.Resize(int(cfg.Cols), int(cfg.Rows))
	}

	subscription := &DataPlaneSubscription{
		process: process,
		id:      1,
		sub:     sub,
		exit:    process.exit,
	}
	return process.dataPlaneInfo(), subscription, nil
}

// ListDataPlaneProcesses returns all currently running Data Plane processes.
func (supervisor *Supervisor) ListDataPlaneProcesses() []DataPlaneProcessInfo {
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	result := make([]DataPlaneProcessInfo, 0)
	for _, process := range supervisor.processes {
		if !process.isDataPlane {
			continue
		}
		info := process.dataPlaneInfo()
		if info.Running {
			result = append(result, info)
		}
	}
	return result
}

// ConnectDataPlaneProcess attaches a new subscription to an existing Data Plane
// process identified by PID or tag.
func (supervisor *Supervisor) ConnectDataPlaneProcess(pid uint32, tag string) (DataPlaneProcessInfo, *DataPlaneSubscription, error) {
	process, err := supervisor.findDataPlaneProcess(pid, tag)
	if err != nil {
		return DataPlaneProcessInfo{}, nil, err
	}
	process.mu.Lock()
	replay := slices.Clone(process.dataPlaneLog)
	running := process.state.Phase == phaseRunning || process.state.Phase == phaseStarting
	var sub *dataPlaneSubscriber
	var subID uint64
	if running {
		process.nextDataPlaneID++
		subID = process.nextDataPlaneID
		sub = &dataPlaneSubscriber{chunks: make(chan DataPlaneChunk, 256)}
		if process.dataPlaneSubs == nil {
			process.dataPlaneSubs = make(map[uint64]*dataPlaneSubscriber)
		}
		process.dataPlaneSubs[subID] = sub
	}
	process.mu.Unlock()

	return process.dataPlaneInfo(), &DataPlaneSubscription{
		process: process,
		id:      subID,
		sub:     sub,
		replay:  replay,
		exit:    process.exit,
	}, nil
}

// SendDataPlaneInput writes bytes to the stdin or PTY of a Data Plane process.
func (supervisor *Supervisor) SendDataPlaneInput(pid uint32, tag string, data []byte) error {
	process, err := supervisor.findDataPlaneProcess(pid, tag)
	if err != nil {
		return err
	}
	process.inputMu.Lock()
	defer process.inputMu.Unlock()
	process.mu.Lock()
	input := process.input
	closed := process.inputClosed
	process.mu.Unlock()
	if input == nil || closed {
		return errors.New("process stdin is closed")
	}
	_, err = input.Write(data)
	return err
}

// CloseDataPlaneStdin closes standard input of a non-PTY Data Plane process.
func (supervisor *Supervisor) CloseDataPlaneStdin(pid uint32, tag string) error {
	process, err := supervisor.findDataPlaneProcess(pid, tag)
	if err != nil {
		return err
	}
	return process.closeInput()
}

// SendDataPlaneSignal sends SIGTERM (force=false) or SIGKILL (force=true) to a
// Data Plane process tree.
func (supervisor *Supervisor) SendDataPlaneSignal(pid uint32, tag string, force bool) error {
	process, err := supervisor.findDataPlaneProcess(pid, tag)
	if err != nil {
		return err
	}
	process.mu.Lock()
	native := process.native
	running := process.state.Phase == phaseRunning || process.state.Phase == phaseStarting
	if running {
		process.stopRequested = true
	}
	process.mu.Unlock()
	if !running || native == nil {
		return nil
	}
	err = native.Stop(force)
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// ResizeDataPlanePTY updates the terminal size of a PTY Data Plane process.
func (supervisor *Supervisor) ResizeDataPlanePTY(pid uint32, tag string, cols, rows uint16) error {
	process, err := supervisor.findDataPlaneProcess(pid, tag)
	if err != nil {
		return err
	}
	return supervisor.Resize(process.processID, "", rows, cols)
}

func (supervisor *Supervisor) findDataPlaneProcess(pid uint32, tag string) (*supervisedProcess, error) {
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	var fallback *supervisedProcess
	for _, process := range supervisor.processes {
		if !process.isDataPlane {
			continue
		}
		process.mu.Lock()
		matchesPID := pid != 0 && uint32(process.state.PID) == pid
		matchesTag := tag != "" && process.dataPlaneTag == tag
		running := process.state.Phase == phaseRunning || process.state.Phase == phaseStarting
		process.mu.Unlock()
		if matchesPID || matchesTag {
			if running {
				return process, nil
			}
			fallback = process
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, processruntime.ErrNotFound
}

func (process *supervisedProcess) dataPlaneInfo() DataPlaneProcessInfo {
	process.mu.Lock()
	defer process.mu.Unlock()
	info := DataPlaneProcessInfo{
		ID:      process.processID,
		PID:     uint32(process.state.PID),
		Tag:     process.dataPlaneTag,
		Running: process.state.Phase == phaseRunning || process.state.Phase == phaseStarting,
	}
	if process.dataPlaneConfig != nil {
		info.Config = *process.dataPlaneConfig
	}
	if !info.Running {
		info.Exited = true
		if process.state.ExitCode != nil {
			info.ExitCode = *process.state.ExitCode
		}
		if info.ExitCode == 0 {
			info.Status = "exit status 0"
		} else {
			info.Status = fmt.Sprintf("exit status %d", info.ExitCode)
		}
		if process.state.Reason != "" && !strings.EqualFold(process.state.Reason, phaseStopped) {
			info.Error = process.state.Reason
		}
	}
	return info
}
