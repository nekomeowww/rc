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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
)

// ErrProcessConflict reports an attempt to reuse one rc ID for another UID.
var ErrProcessConflict = errors.New("process ID belongs to another UID")

// ErrSlowClient reports an attach client that could not keep up with output.
var ErrSlowClient = errors.New("attach client was disconnected because it could not keep up with process output")

const (
	phaseStarting = "Starting"
	phaseRunning  = "Running"
	phaseExited   = "Exited"
)

type supervisedProcess struct {
	mu                  sync.Mutex
	inputMu             sync.Mutex
	state               processruntime.State
	command             *exec.Cmd
	input               io.WriteCloser
	terminal            *os.File
	transcript          *os.File
	transcriptPath      string
	exit                chan struct{}
	stopRequested       bool
	clients             map[string]chan []byte
	clientSizes         map[string]pty.Winsize
	nextClient          uint64
	foregroundClient    string
	transcriptMax       int64
	transcriptLen       int64
	truncated           bool
	runtimeDir          string
	credentialLinks     []string
	agentCredentialLink string
	supervisor          *Supervisor
	processID           string
}

// Supervisor owns child processes independently of attach connections.
type Supervisor struct {
	mu        sync.RWMutex
	stateDir  string
	stopGrace time.Duration
	processes map[string]*supervisedProcess
	maxLog    int64
	aliases   map[string]*credentialAlias
}

type credentialAlias struct {
	target string
	owners map[string]struct{}
}

// SupervisorOption changes a supervisor resource bound.
type SupervisorOption func(*Supervisor)

// WithMaxTranscriptBytes bounds each durable transcript. Zero disables the
// bound; live attached clients still receive output after truncation.
func WithMaxTranscriptBytes(limit int64) SupervisorOption {
	return func(supervisor *Supervisor) { supervisor.maxLog = limit }
}

// NewSupervisor creates a process owner whose durable records live in stateDir.
func NewSupervisor(stateDir string, stopGrace time.Duration, options ...SupervisorOption) *Supervisor {
	supervisor := &Supervisor{stateDir: stateDir, stopGrace: stopGrace, processes: make(map[string]*supervisedProcess), maxLog: 64 << 20, aliases: make(map[string]*credentialAlias)}
	for _, option := range options {
		option(supervisor)
	}
	return supervisor
}

// Start launches a command once for the (ID, UID) identity.
//
//nolint:gocyclo // Start is an ordered ownership, filesystem, PTY, and process transaction.
func (supervisor *Supervisor) Start(request processruntime.StartRequest) (processruntime.State, error) {
	if request.ID == "" || request.UID == "" {
		return processruntime.State{}, errors.New("process ID and UID are required")
	}
	if filepath.Base(request.ID) != request.ID || request.ID == "." || request.ID == ".." {
		return processruntime.State{}, errors.New("process ID must be one safe path segment")
	}
	if len(request.Command) == 0 || request.Command[0] == "" {
		return processruntime.State{}, errors.New("process command is required")
	}

	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if existing := supervisor.processes[request.ID]; existing != nil {
		existing.mu.Lock()
		defer existing.mu.Unlock()
		if existing.state.UID != request.UID {
			return processruntime.State{}, ErrProcessConflict
		}

		return cloneState(existing.state), nil
	}

	stateDirectory := filepath.Join(supervisor.stateDir, request.ID)
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return processruntime.State{}, fmt.Errorf("create persistent process state directory: %w", err)
	}
	ownerPath := filepath.Join(stateDirectory, "owner.uid")
	if ownerUID, err := os.ReadFile(ownerPath); err == nil {
		if string(ownerUID) != request.UID {
			return processruntime.State{}, ErrProcessConflict
		}
		return processruntime.State{}, processruntime.ErrNotFound
	} else if !errors.Is(err, os.ErrNotExist) {
		return processruntime.State{}, fmt.Errorf("read persistent process ownership: %w", err)
	}
	if err := os.WriteFile(ownerPath, []byte(request.UID), 0o600); err != nil {
		return processruntime.State{}, fmt.Errorf("record persistent process ownership: %w", err)
	}
	commandLaunched := false
	defer func() {
		if !commandLaunched {
			_ = os.Remove(ownerPath)
		}
	}()
	runtimeDirectory := request.RuntimeDirectory
	if runtimeDirectory == "" {
		runtimeDirectory = filepath.Join(os.TempDir(), "rc-kube-processes", request.ID)
	}
	if filepath.Base(runtimeDirectory) != request.ID {
		return processruntime.State{}, errors.New("process runtime directory must end with the process ID")
	}
	if err := os.MkdirAll(runtimeDirectory, 0o700); err != nil {
		return processruntime.State{}, fmt.Errorf("create temporary process runtime directory: %w", err)
	}
	transcriptPath := request.TranscriptPath
	if transcriptPath == "" {
		transcriptPath = filepath.Join(stateDirectory, "transcript.log")
	}
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o700); err != nil {
		return processruntime.State{}, fmt.Errorf("create transcript directory: %w", err)
	}
	transcript, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return processruntime.State{}, fmt.Errorf("open process transcript: %w", err)
	}
	if err := writeCredentialFiles(runtimeDirectory, request.CredentialFiles); err != nil {
		_ = transcript.Close()
		return processruntime.State{}, err
	}
	credentialLinks, err := supervisor.prepareCredentialAliases(request.ID, runtimeDirectory, request.CredentialsRoot, request.CredentialFiles)
	if err != nil {
		_ = transcript.Close()
		return processruntime.State{}, err
	}
	mountedCredentialLinks, err := supervisor.prepareCredentialMounts(request.ID, runtimeDirectory, request.CredentialMounts)
	if err != nil {
		_ = transcript.Close()
		supervisor.releaseCredentialAliasesLocked(request.ID, credentialLinks, "")
		_ = os.RemoveAll(runtimeDirectory)
		return processruntime.State{}, err
	}
	credentialLinks = append(credentialLinks, mountedCredentialLinks...)
	agentCredentialLink, err := supervisor.prepareAgentHome(request.ID, runtimeDirectory, request.AgentHome, request.CredentialFiles)
	if err != nil {
		_ = transcript.Close()
		supervisor.releaseCredentialAliasesLocked(request.ID, credentialLinks, "")
		_ = os.RemoveAll(runtimeDirectory)
		return processruntime.State{}, err
	}
	defer func() {
		if !commandLaunched {
			supervisor.releaseCredentialAliasesLocked(request.ID, credentialLinks, agentCredentialLink)
			_ = os.RemoveAll(runtimeDirectory)
		}
	}()

	command := exec.Command(request.Command[0], request.Command[1:]...)
	command.Dir = request.WorkingDirectory
	processEnvironment := maps.Clone(request.Environment)
	if request.TTY {
		if processEnvironment == nil {
			processEnvironment = make(map[string]string)
		}
		if _, configured := processEnvironment["TERM"]; !configured {
			processEnvironment["TERM"] = "xterm-256color"
		}
	}
	command.Env = mergedEnvironment(processEnvironment)
	transcriptInfo, err := transcript.Stat()
	if err != nil {
		_ = transcript.Close()
		return processruntime.State{}, fmt.Errorf("stat process transcript: %w", err)
	}
	process := &supervisedProcess{
		state:   processruntime.State{ID: request.ID, UID: request.UID, Phase: phaseStarting},
		command: command, transcript: transcript, transcriptPath: transcriptPath, exit: make(chan struct{}), clients: make(map[string]chan []byte), clientSizes: make(map[string]pty.Winsize),
		transcriptMax: supervisor.maxLog, transcriptLen: transcriptInfo.Size(),
		runtimeDir: runtimeDirectory, credentialLinks: credentialLinks, agentCredentialLink: agentCredentialLink,
		supervisor: supervisor, processID: request.ID,
	}
	output := processOutput{process: process}
	var outputDone chan struct{}
	if request.TTY {
		terminal, startErr := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 80})
		if startErr != nil {
			if failed, handled, failureErr := supervisor.recordCommandStartFailureLocked(process, startErr); handled {
				commandLaunched = failureErr == nil
				return failed, failureErr
			}
			_ = transcript.Close()
			return processruntime.State{}, fmt.Errorf("start PTY process: %w", startErr)
		}
		commandLaunched = true
		process.terminal = terminal
		process.input = terminal
		outputDone = make(chan struct{})
		go func() {
			defer close(outputDone)
			_, _ = io.Copy(output, terminal)
		}()
	} else {
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		input, inputErr := command.StdinPipe()
		if inputErr != nil {
			_ = transcript.Close()
			return processruntime.State{}, fmt.Errorf("open process stdin: %w", inputErr)
		}
		process.input = input
		command.Stdout = output
		command.Stderr = output
		if startErr := command.Start(); startErr != nil {
			_ = input.Close()
			if failed, handled, failureErr := supervisor.recordCommandStartFailureLocked(process, startErr); handled {
				commandLaunched = failureErr == nil
				return failed, failureErr
			}
			_ = transcript.Close()
			return processruntime.State{}, fmt.Errorf("start process: %w", startErr)
		}
		commandLaunched = true
	}
	process.mu.Lock()
	process.state.Phase = phaseRunning
	process.state.PID = command.Process.Pid
	started := cloneState(process.state)
	process.mu.Unlock()
	supervisor.processes[request.ID] = process
	go process.wait(outputDone)

	return started, nil
}

func (supervisor *Supervisor) recordCommandStartFailureLocked(process *supervisedProcess, startErr error) (processruntime.State, bool, error) {
	exitCode, terminal := commandStartFailureExitCode(startErr)
	if !terminal {
		return processruntime.State{}, false, nil
	}
	message := fmt.Appendf(nil, "rc-kube: command could not start: %v\n", startErr)
	if _, err := (processOutput{process: process}).Write(message); err != nil {
		return processruntime.State{}, true, fmt.Errorf("record command start failure: %w", err)
	}
	if err := process.transcript.Sync(); err != nil {
		return processruntime.State{}, true, fmt.Errorf("sync command start failure: %w", err)
	}
	if err := process.transcript.Close(); err != nil {
		return processruntime.State{}, true, fmt.Errorf("close command start failure transcript: %w", err)
	}
	process.transcript = nil
	process.state.Phase = phaseExited
	process.state.ExitCode = &exitCode
	process.state.Reason = startErr.Error()
	close(process.exit)
	supervisor.processes[process.processID] = process
	supervisor.releaseCredentialAliasesLocked(process.processID, process.credentialLinks, process.agentCredentialLink)
	_ = os.RemoveAll(process.runtimeDir)

	return cloneState(process.state), true, nil
}

func commandStartFailureExitCode(err error) (int32, bool) {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return 127, true
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ENOEXEC) {
		return 126, true
	}

	return 0, false
}

// Inspect returns the latest state retained by this supervisor instance.
func (supervisor *Supervisor) Inspect(id string) (processruntime.State, error) {
	process, err := supervisor.process(id)
	if err != nil {
		return processruntime.State{}, err
	}
	process.mu.Lock()
	defer process.mu.Unlock()

	return cloneState(process.state), nil
}

// Stop terminates the whole child process group, escalating after stopGrace.
func (supervisor *Supervisor) Stop(id string) (processruntime.State, error) {
	process, err := supervisor.process(id)
	if err != nil {
		return processruntime.State{}, err
	}
	process.mu.Lock()
	if process.state.Phase != phaseRunning && process.state.Phase != phaseStarting {
		state := cloneState(process.state)
		process.mu.Unlock()
		return state, nil
	}
	process.stopRequested = true
	pid := process.state.PID
	exit := process.exit
	process.mu.Unlock()

	if err := signalProcessGroup(pid, syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return processruntime.State{}, fmt.Errorf("send SIGTERM to process group: %w", err)
	}
	timer := time.NewTimer(supervisor.stopGrace)
	defer timer.Stop()
	select {
	case <-exit:
	case <-timer.C:
		if err := signalProcessGroup(pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
			return processruntime.State{}, fmt.Errorf("send SIGKILL to process group: %w", err)
		}
		<-exit
	}

	return supervisor.Inspect(id)
}

// Logs copies the durable transcript without requiring a live process.
func (supervisor *Supervisor) Logs(id string, output io.Writer) error {
	process, err := supervisor.process(id)
	if err != nil {
		path := filepath.Join(supervisor.stateDir, id, "transcript.log")
		file, openErr := os.Open(path)
		if openErr != nil {
			if errors.Is(openErr, os.ErrNotExist) {
				return processruntime.ErrNotFound
			}
			return fmt.Errorf("open persisted transcript: %w", openErr)
		}
		defer func() { _ = file.Close() }()
		_, copyErr := io.Copy(output, file)
		return copyErr
	}
	process.mu.Lock()
	data, err := process.readTranscriptLocked()
	process.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = output.Write(data)

	return err
}

// TranscriptAvailable validates a logs request before the streaming protocol
// sends a success response.
func (supervisor *Supervisor) TranscriptAvailable(id string) error {
	if filepath.Base(id) != id || id == "." || id == ".." {
		return processruntime.ErrNotFound
	}
	if _, err := supervisor.process(id); err == nil {
		return nil
	}
	_, err := os.Stat(filepath.Join(supervisor.stateDir, id, "transcript.log"))
	if errors.Is(err, os.ErrNotExist) {
		return processruntime.ErrNotFound
	}
	return err
}

// Attach replays the raw transcript and then follows the raw PTY output. Input
// is shared with every other attached client.
func (supervisor *Supervisor) Attach(ctx context.Context, id string, clientID string, input io.Reader, output io.Writer, rows uint16, columns uint16) error {
	process, err := supervisor.process(id)
	if err != nil {
		return err
	}
	process.mu.Lock()
	replay, err := process.readTranscriptLocked()
	if err != nil {
		process.mu.Unlock()
		return err
	}
	if process.state.Phase != phaseRunning && process.state.Phase != phaseStarting {
		process.mu.Unlock()
		_, err := output.Write(replay)
		return err
	}
	if clientID == "" {
		clientID = fmt.Sprintf("client-%d", process.nextClient)
		process.nextClient++
	}
	if _, exists := process.clients[clientID]; exists {
		process.mu.Unlock()
		return errors.New("attach client ID is already connected")
	}
	stream := make(chan []byte, 128)
	process.clients[clientID] = stream
	if process.foregroundClient == "" {
		process.foregroundClient = clientID
	}
	if rows > 0 && columns > 0 {
		size := pty.Winsize{Rows: rows, Cols: columns}
		process.clientSizes[clientID] = size
		if process.foregroundClient == clientID && process.terminal != nil {
			if err := pty.Setsize(process.terminal, &size); err != nil {
				delete(process.clients, clientID)
				delete(process.clientSizes, clientID)
				process.foregroundClient = ""
				process.mu.Unlock()
				return fmt.Errorf("set initial PTY size: %w", err)
			}
		}
	}
	process.state.AttachedClients = int32(len(process.clients))
	exit := process.exit
	processInput := process.input
	process.mu.Unlock()
	defer func() {
		process.mu.Lock()
		delete(process.clients, clientID)
		delete(process.clientSizes, clientID)
		if process.foregroundClient == clientID {
			process.foregroundClient = ""
		}
		process.state.AttachedClients = int32(len(process.clients))
		process.mu.Unlock()
	}()
	if _, err := output.Write(replay); err != nil {
		return err
	}
	if input != nil {
		go func() {
			_, _ = io.Copy(foregroundWriter{process: process, clientID: clientID, target: processInput}, input)
		}()
	}
	for {
		select {
		case data, ok := <-stream:
			if !ok {
				return ErrSlowClient
			}
			if _, err := output.Write(data); err != nil {
				return err
			}
		case <-exit:
			for {
				select {
				case data, ok := <-stream:
					if !ok {
						return nil
					}
					if _, err := output.Write(data); err != nil {
						return err
					}
				default:
					return nil
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Resize updates a PTY's canonical size. Non-terminal processes reject it.
func (supervisor *Supervisor) Resize(id string, clientID string, rows uint16, columns uint16) error {
	process, err := supervisor.process(id)
	if err != nil {
		return err
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	if process.terminal == nil {
		return errors.New("process does not own a PTY")
	}
	if clientID != "" {
		if process.clientSizes == nil {
			process.clientSizes = make(map[string]pty.Winsize)
		}
		process.clientSizes[clientID] = pty.Winsize{Rows: rows, Cols: columns}
		if process.foregroundClient == "" {
			process.foregroundClient = clientID
		}
		if process.foregroundClient != clientID {
			return nil
		}
	}
	return pty.Setsize(process.terminal, &pty.Winsize{Rows: rows, Cols: columns})
}

func (supervisor *Supervisor) process(id string) (*supervisedProcess, error) {
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	process := supervisor.processes[id]
	if process == nil {
		return nil, processruntime.ErrNotFound
	}

	return process, nil
}

func (process *supervisedProcess) wait(outputDone <-chan struct{}) {
	err := process.command.Wait()
	if process.terminal != nil {
		_ = process.terminal.Close()
	} else if process.input != nil {
		_ = process.input.Close()
	}
	if outputDone != nil {
		<-outputDone
	}
	process.cleanupCredentials()
	exitCode := int32(-1)
	if process.command.ProcessState != nil {
		exitCode = int32(process.command.ProcessState.ExitCode())
	}
	process.mu.Lock()
	process.state.ExitCode = &exitCode
	if process.stopRequested {
		process.state.Phase = "Stopped"
		process.state.Reason = "Stopped"
	} else {
		process.state.Phase = phaseExited
		if err != nil {
			process.state.Reason = err.Error()
		}
	}
	if process.transcript != nil {
		_ = process.transcript.Sync()
		_ = process.transcript.Close()
		process.transcript = nil
	}
	close(process.exit)
	process.mu.Unlock()
}

func (process *supervisedProcess) readTranscriptLocked() ([]byte, error) {
	if process.transcript == nil {
		data, err := os.ReadFile(process.transcriptPath)
		if err != nil {
			return nil, fmt.Errorf("read persisted process transcript: %w", err)
		}
		return data, nil
	}
	if _, err := process.transcript.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek process transcript: %w", err)
	}
	data, err := io.ReadAll(process.transcript)
	if err != nil {
		return nil, fmt.Errorf("read process transcript: %w", err)
	}
	if _, err := process.transcript.Seek(0, io.SeekEnd); err != nil {
		return nil, fmt.Errorf("restore process transcript position: %w", err)
	}
	return data, nil
}

func (process *supervisedProcess) cleanupCredentials() {
	if process.supervisor != nil {
		process.supervisor.mu.Lock()
		process.supervisor.releaseCredentialAliasesLocked(process.processID, process.credentialLinks, process.agentCredentialLink)
		process.supervisor.mu.Unlock()
	}
	_ = os.RemoveAll(process.runtimeDir)
}

type processOutput struct {
	process *supervisedProcess
}

func (output processOutput) Write(data []byte) (int, error) {
	copyOfData := append([]byte(nil), data...)
	output.process.mu.Lock()
	defer output.process.mu.Unlock()
	if !output.process.truncated {
		toWrite := data
		if output.process.transcriptMax > 0 {
			remaining := output.process.transcriptMax - output.process.transcriptLen
			if remaining < int64(len(toWrite)) {
				if remaining < 0 {
					remaining = 0
				}
				toWrite = toWrite[:remaining]
			}
		}
		if len(toWrite) > 0 {
			written, err := output.process.transcript.Write(toWrite)
			output.process.transcriptLen += int64(written)
			if err != nil {
				return 0, err
			}
		}
		if output.process.transcriptMax > 0 && len(toWrite) < len(data) {
			if _, err := output.process.transcript.WriteString("\n[rc-kube transcript truncated]\n"); err != nil {
				return 0, err
			}
			output.process.truncated = true
		}
	}
	for id, client := range output.process.clients {
		select {
		case client <- copyOfData:
		default:
			delete(output.process.clients, id)
			delete(output.process.clientSizes, id)
			if output.process.foregroundClient == id {
				output.process.foregroundClient = ""
			}
			close(client)
		}
	}
	output.process.state.AttachedClients = int32(len(output.process.clients))

	return len(data), nil
}

type foregroundWriter struct {
	process  *supervisedProcess
	clientID string
	target   io.Writer
}

func (writer foregroundWriter) Write(data []byte) (int, error) {
	writer.process.inputMu.Lock()
	defer writer.process.inputMu.Unlock()
	writer.process.mu.Lock()
	writer.process.foregroundClient = writer.clientID
	size, hasSize := writer.process.clientSizes[writer.clientID]
	terminal := writer.process.terminal
	writer.process.mu.Unlock()
	if hasSize && terminal != nil {
		_ = pty.Setsize(terminal, &size)
	}
	return writer.target.Write(data)
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}

	return syscall.Kill(-pid, signal)
}

func mergedEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		for index := 0; index < len(entry); index++ {
			if entry[index] == '=' {
				values[entry[:index]] = entry[index+1:]
				break
			}
		}
	}
	maps.Copy(values, overrides)
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}

	return environment
}

func writeCredentialFiles(processDirectory string, files map[string][]byte) error {
	for name, data := range files {
		path, err := safeChildPath(processDirectory, name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create credential directory: %w", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("write credential file: %w", err)
		}
	}

	return nil
}

func (supervisor *Supervisor) prepareCredentialAliases(processID string, processDirectory string, credentialsRoot string, files map[string][]byte) ([]string, error) {
	if credentialsRoot == "" {
		return nil, nil
	}
	if err := os.MkdirAll(credentialsRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create generic credentials root: %w", err)
	}
	credentialNames := make(map[string]struct{})
	links := make([]string, 0)
	for name := range files {
		parts := strings.Split(filepath.ToSlash(name), "/")
		if len(parts) >= 2 && parts[0] == "credentials" {
			credentialNames[parts[1]] = struct{}{}
		}
	}
	for name := range credentialNames {
		source, err := safeChildPath(processDirectory, filepath.Join("credentials", name))
		if err != nil {
			return nil, err
		}
		link, err := safeChildPath(credentialsRoot, name)
		if err != nil {
			return nil, err
		}
		target := filepath.Join(filepath.Dir(credentialsRoot), "credential-sets", name)
		if err := supervisor.claimCredentialAliasLocked(processID, link, target, func() error {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			return os.CopyFS(target, os.DirFS(source))
		}); err != nil {
			return nil, fmt.Errorf("project generic credential directory: %w", err)
		}
		links = append(links, link)
	}

	return links, nil
}

func (supervisor *Supervisor) prepareCredentialMounts(processID string, processDirectory string, mounts []processruntime.CredentialMount) ([]string, error) {
	links := make([]string, 0, len(mounts))
	completed := false
	defer func() {
		if !completed {
			supervisor.releaseCredentialAliasesLocked(processID, links, "")
		}
	}()
	for _, mount := range mounts {
		source, err := safeChildPath(processDirectory, mount.Source)
		if err != nil {
			return nil, fmt.Errorf("resolve credential mount source: %w", err)
		}
		if !filepath.IsAbs(mount.Target) || filepath.Clean(mount.Target) != mount.Target || mount.Target == string(filepath.Separator) {
			return nil, fmt.Errorf("credential mount target %q must be a clean absolute file path", mount.Target)
		}
		if err := os.MkdirAll(filepath.Dir(mount.Target), 0o700); err != nil {
			return nil, fmt.Errorf("create credential mount directory: %w", err)
		}
		sum := sha256.Sum256([]byte(mount.Target))
		targetDirectory := filepath.Join(filepath.Dir(processDirectory), "credential-mounts", hex.EncodeToString(sum[:10]))
		target, err := safeChildPath(targetDirectory, filepath.Base(mount.Target))
		if err != nil {
			return nil, err
		}
		if err := supervisor.claimCredentialAliasLocked(processID, mount.Target, target, func() error {
			data, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			return os.WriteFile(target, data, 0o600)
		}); err != nil {
			return nil, fmt.Errorf("project credential file to %s: %w", mount.Target, err)
		}
		links = append(links, mount.Target)
	}
	completed = true
	return links, nil
}

func (supervisor *Supervisor) prepareAgentHome(processID string, processDirectory string, agentHome string, files map[string][]byte) (string, error) {
	if agentHome == "" {
		return "", nil
	}
	if err := os.MkdirAll(agentHome, 0o700); err != nil {
		return "", fmt.Errorf("create Agent home: %w", err)
	}
	if _, ok := files["agent/auth.json"]; !ok {
		return "", nil
	}
	source := filepath.Join(processDirectory, "agent", "auth.json")
	link := filepath.Join(agentHome, "auth.json")
	sum := sha256.Sum256([]byte(agentHome))
	target := filepath.Join(filepath.Dir(processDirectory), "agent-credentials", hex.EncodeToString(sum[:10]), "auth.json")
	if err := supervisor.claimCredentialAliasLocked(processID, link, target, func() error {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	}); err != nil {
		return "", fmt.Errorf("project Agent credential: %w", err)
	}

	return link, nil
}

func (supervisor *Supervisor) claimCredentialAliasLocked(processID string, link string, target string, prepare func() error) error {
	if existing := supervisor.aliases[link]; existing != nil {
		existing.owners[processID] = struct{}{}
		return nil
	}
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("credential projection target %s already exists and is not a symbolic link", link)
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := prepare(); err != nil {
		return err
	}
	if err := os.Symlink(target, link); err != nil {
		_ = os.RemoveAll(target)
		return err
	}
	supervisor.aliases[link] = &credentialAlias{target: target, owners: map[string]struct{}{processID: {}}}
	return nil
}

func (supervisor *Supervisor) releaseCredentialAliasesLocked(processID string, credentialLinks []string, agentCredentialLink string) {
	links := append([]string(nil), credentialLinks...)
	if agentCredentialLink != "" {
		links = append(links, agentCredentialLink)
	}
	for _, link := range links {
		alias := supervisor.aliases[link]
		if alias == nil {
			continue
		}
		delete(alias.owners, processID)
		if len(alias.owners) != 0 {
			continue
		}
		if target, err := os.Readlink(link); err == nil && target == alias.target {
			_ = os.Remove(link)
		}
		_ = os.RemoveAll(alias.target)
		delete(supervisor.aliases, link)
	}
}

func safeChildPath(parent string, name string) (string, error) {
	path := filepath.Join(parent, name)
	relative, err := filepath.Rel(parent, path)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("credential path %q escapes process directory", name)
	}

	return path, nil
}

func cloneState(state processruntime.State) processruntime.State {
	clone := state
	if state.ExitCode != nil {
		exitCode := *state.ExitCode
		clone.ExitCode = &exitCode
	}

	return clone
}
