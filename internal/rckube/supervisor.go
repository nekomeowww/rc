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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
	"github.com/nekomeowww/rc/internal/osprocess"
)

// ErrProcessConflict reports an attempt to reuse one rc ID for another UID.
var ErrProcessConflict = errors.New("process ID belongs to another UID")

// ErrSlowClient reports an attach client that could not keep up with output.
var ErrSlowClient = errors.New("attach client was disconnected because it could not keep up with process output")

const (
	runtimeOSWindows             = "windows"
	phaseStarting                = "Starting"
	phaseRunning                 = "Running"
	phaseExited                  = "Exited"
	sshIdentityFilePlaceholder   = "${identityFile}"
	sshKnownHostsFilePlaceholder = "${knownHostsFile}"
)

type terminalSize struct{ Rows, Cols uint16 }

type terminalController interface {
	Resize(columns, rows int) error
}

type supervisedProcess struct {
	mu                  sync.Mutex
	inputMu             sync.Mutex
	state               processruntime.State
	native              *osprocess.Process
	input               io.WriteCloser
	terminal            terminalController
	transcript          *os.File
	transcriptPath      string
	exit                chan struct{}
	stopRequested       bool
	clients             map[string]chan []byte
	clientSizes         map[string]terminalSize
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
	mu           sync.RWMutex
	stateDir     string
	homeDir      string
	workspaceDir string
	runtimeRoot  string
	stopGrace    time.Duration
	processes    map[string]*supervisedProcess
	maxLog       int64
	aliases      map[string]*credentialAlias
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

// WithRoots sets the native directories used to derive process-owned paths.
func WithRoots(home, workspace, runtimeRoot string) SupervisorOption {
	return func(supervisor *Supervisor) {
		supervisor.homeDir = home
		supervisor.workspaceDir = workspace
		supervisor.runtimeRoot = runtimeRoot
	}
}

// NewSupervisor creates a process owner whose durable records live in stateDir.
func NewSupervisor(stateDir string, stopGrace time.Duration, options ...SupervisorOption) *Supervisor {
	supervisor := &Supervisor{
		stateDir: stateDir, runtimeRoot: filepath.Join(os.TempDir(), "rc-kube"), stopGrace: stopGrace,
		processes: make(map[string]*supervisedProcess), maxLog: 64 << 20, aliases: make(map[string]*credentialAlias),
	}
	for _, option := range options {
		option(supervisor)
	}
	return supervisor
}

// Shutdown stops owned processes concurrently so one slow child cannot consume
// the entire Pod termination grace period before other trees receive a stop.
func (supervisor *Supervisor) Shutdown() {
	supervisor.mu.RLock()
	ids := make([]string, 0, len(supervisor.processes))
	for id := range supervisor.processes {
		ids = append(ids, id)
	}
	supervisor.mu.RUnlock()
	var pending sync.WaitGroup
	for _, id := range ids {
		pending.Go(func() { _, _ = supervisor.Stop(id) })
	}
	pending.Wait()
}

// Start launches a command once for the (ID, UID) identity.
// ctx governs preparation; launched children remain owned by the supervisor.
// The protocol server passes its service context, so client disconnection does
// not cancel an accepted start. Shutdown must run after pending starts finish.
func (supervisor *Supervisor) Start(ctx context.Context, request processruntime.StartRequest) (processruntime.State, error) {
	if err := ctx.Err(); err != nil {
		return processruntime.State{}, err
	}
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
	if err := ctx.Err(); err != nil {
		return processruntime.State{}, err
	}
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
	attempt := &processStart{
		supervisor: supervisor, ownerPath: ownerPath,
		process: &supervisedProcess{
			state: processruntime.State{ID: request.ID, UID: request.UID, Phase: phaseStarting},
			exit:  make(chan struct{}), clients: make(map[string]chan []byte), clientSizes: make(map[string]terminalSize),
			transcriptMax: supervisor.maxLog, supervisor: supervisor, processID: request.ID,
		},
	}
	return attempt.runLocked(ctx, request)
}

func (supervisor *Supervisor) recordCommandStartFailureLocked(process *supervisedProcess, startErr error, exitCode int32) (processruntime.State, error) {
	message := fmt.Appendf(nil, "rc-kube: command could not start: %v\n", startErr)
	if _, err := (processOutput{process: process}).Write(message); err != nil {
		return processruntime.State{}, fmt.Errorf("record command start failure: %w", err)
	}
	if err := process.transcript.Sync(); err != nil {
		return processruntime.State{}, fmt.Errorf("sync command start failure: %w", err)
	}
	if err := process.transcript.Close(); err != nil {
		return processruntime.State{}, fmt.Errorf("close command start failure transcript: %w", err)
	}
	process.transcript = nil
	process.state.Phase = phaseExited
	process.state.ExitCode = &exitCode
	process.state.Reason = startErr.Error()
	close(process.exit)
	supervisor.processes[process.processID] = process
	return cloneState(process.state), nil
}

func commandStartFailureExitCode(err error) (int32, bool) {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return 127, true
	}
	if errors.Is(err, os.ErrPermission) || osprocess.InvalidExecutable(err) {
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
	native := process.native
	exit := process.exit
	process.mu.Unlock()

	if err := native.Stop(false); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return processruntime.State{}, fmt.Errorf("request process tree stop: %w", err)
	}
	timer := time.NewTimer(supervisor.stopGrace)
	defer timer.Stop()
	select {
	case <-exit:
	case <-timer.C:
		if err := native.Stop(true); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
			return processruntime.State{}, fmt.Errorf("terminate process tree: %w", err)
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
		size := terminalSize{Rows: rows, Cols: columns}
		process.clientSizes[clientID] = size
		if process.foregroundClient == clientID && process.terminal != nil {
			if err := process.terminal.Resize(int(size.Cols), int(size.Rows)); err != nil {
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
			process.clientSizes = make(map[string]terminalSize)
		}
		process.clientSizes[clientID] = terminalSize{Rows: rows, Cols: columns}
		if process.foregroundClient == "" {
			process.foregroundClient = clientID
		}
		if process.foregroundClient != clientID {
			return nil
		}
	}
	return process.terminal.Resize(int(columns), int(rows))
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

func (process *supervisedProcess) wait() {
	err := process.native.Wait()
	process.cleanupCredentials()
	exitCode := int32(-1)
	if process.native.ProcessState() != nil {
		exitCode = int32(process.native.ProcessState().ExitCode())
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
		_ = terminal.Resize(int(size.Cols), int(size.Rows))
	}
	return writer.target.Write(data)
}

func mergedEnvironment(overrides map[string]string) ([]string, error) {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		for index := 0; index < len(entry); index++ {
			if entry[index] == '=' {
				name := entry[:index]
				if runtime.GOOS == runtimeOSWindows {
					name = strings.ToUpper(name)
				}
				values[name] = entry[index+1:]
				break
			}
		}
	}
	overrideNames := make([]string, 0, len(overrides))
	for name := range overrides {
		overrideNames = append(overrideNames, name)
	}
	slices.Sort(overrideNames)
	seenOverrides := make(map[string]string, len(overrides))
	for _, originalName := range overrideNames {
		name := originalName
		if runtime.GOOS == runtimeOSWindows {
			name = strings.ToUpper(name)
		}
		if previous, exists := seenOverrides[name]; exists {
			return nil, fmt.Errorf("environment variables %s and %s differ only by case", previous, originalName)
		}
		seenOverrides[name] = originalName
		values[name] = overrides[originalName]
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}

	return environment, nil
}

func writeCredentialFiles(processDirectory string, files map[string][]byte) error {
	root, err := os.OpenRoot(processDirectory)
	if err != nil {
		return fmt.Errorf("open process credential root: %w", err)
	}
	defer func() { _ = root.Close() }()
	for name, data := range files {
		localName, err := filepath.Localize(name)
		if err != nil {
			return fmt.Errorf("credential path %q must be a portable relative path: %w", name, err)
		}
		if err := root.MkdirAll(filepath.Dir(localName), 0o700); err != nil {
			return fmt.Errorf("create credential directory: %w", err)
		}
		if err := root.WriteFile(localName, data, 0o600); err != nil {
			return fmt.Errorf("write credential file: %w", err)
		}
	}

	return nil
}

func copyCredentialDirectory(target string, source string) error {
	sourceFS := os.DirFS(source)
	return fs.WalkDir(sourceFS, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		destination, err := safeChildPath(target, filepath.FromSlash(name))
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := os.MkdirAll(destination, 0o700); err != nil {
				return fmt.Errorf("create private credential directory: %w", err)
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect credential file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("credential path %q is not a regular file", name)
		}
		data, err := fs.ReadFile(sourceFS, name)
		if err != nil {
			return fmt.Errorf("read credential file: %w", err)
		}
		if err := os.WriteFile(destination, data, 0o600); err != nil {
			return fmt.Errorf("write private credential file: %w", err)
		}

		return nil
	})
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
			if err := copyCredentialDirectory(target, source); err != nil {
				_ = os.RemoveAll(target)
				return err
			}
			return nil
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

func (supervisor *Supervisor) prepareSSHConfig(processID string, processDirectory string, credentialsRoot string, configPath string, fragments map[string]string) ([]string, error) {
	if len(fragments) == 0 {
		return nil, nil
	}
	if credentialsRoot == "" {
		return nil, errors.New("SSH config fragments require a credentials root")
	}
	if !filepath.IsAbs(configPath) || filepath.Clean(configPath) != configPath || configPath == string(filepath.Separator) {
		return nil, fmt.Errorf("SSH config path %q must be a clean absolute file path", configPath)
	}
	fragmentDirectory := configPath + ".d"
	includePattern := filepath.ToSlash(filepath.Join(fragmentDirectory, "*.conf"))
	if err := ensureSSHConfigInclude(configPath, includePattern); err != nil {
		return nil, fmt.Errorf("ensure managed SSH config include: %w", err)
	}
	if err := os.MkdirAll(fragmentDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create managed SSH config fragment directory: %w", err)
	}

	names := make([]string, 0, len(fragments))
	for name := range fragments {
		names = append(names, name)
	}
	slices.Sort(names)
	links := make([]string, 0, len(names))
	completed := false
	defer func() {
		if !completed {
			supervisor.releaseCredentialAliasesLocked(processID, links, "")
		}
	}()
	for _, name := range names {
		if filepath.Base(name) != name || name == "." || name == ".." {
			return nil, fmt.Errorf("SSH config fragment name %q must be one safe path segment", name)
		}
		identityFile := filepath.ToSlash(filepath.Join(credentialsRoot, name, "id"))
		knownHostsFile := filepath.ToSlash(filepath.Join(credentialsRoot, name, "known_hosts"))
		content := strings.ReplaceAll(fragments[name], sshIdentityFilePlaceholder, identityFile)
		content = strings.ReplaceAll(content, sshKnownHostsFilePlaceholder, knownHostsFile)
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}

		link := filepath.Join(fragmentDirectory, "rc-"+name+".conf")
		sum := sha256.Sum256([]byte(link))
		targetDirectory := filepath.Join(filepath.Dir(processDirectory), "ssh-config-fragments", hex.EncodeToString(sum[:10]))
		target := filepath.Join(targetDirectory, filepath.Base(link))
		if err := supervisor.claimCredentialAliasLocked(processID, link, target, func() error {
			if err := os.MkdirAll(targetDirectory, 0o700); err != nil {
				return err
			}
			return os.WriteFile(target, []byte(content), 0o600)
		}); err != nil {
			return nil, fmt.Errorf("project SSH config fragment %s: %w", name, err)
		}
		links = append(links, link)
	}
	completed = true
	return links, nil
}

func ensureSSHConfigInclude(configPath string, includePattern string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	data, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	managedBlock := "# rc: managed SSH Credential fragments\nHost *\n  Include " + includePattern + "\n"
	if strings.Contains(string(data), managedBlock) {
		return nil
	}
	file, err := os.OpenFile(configPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	prefix := ""
	if len(data) > 0 && data[len(data)-1] != '\n' {
		prefix = "\n"
	}
	if _, err := file.WriteString(prefix + managedBlock); err != nil {
		return err
	}
	return file.Sync()
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
