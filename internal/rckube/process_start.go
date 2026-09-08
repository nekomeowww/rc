package rckube

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
	"github.com/nekomeowww/rc/internal/osprocess"
)

// processStart owns resources until either a running process or a recorded
// failure is committed. Its methods run synchronously under supervisor.mu.
type processStart struct {
	supervisor *Supervisor
	process    *supervisedProcess
	ownerPath  string
}

func (attempt *processStart) runLocked(ctx context.Context, request processruntime.StartRequest) (processruntime.State, error) {
	command, err := attempt.prepare(ctx, request)
	if err != nil {
		return attempt.failLocked(err, 125)
	}
	return attempt.launchLocked(ctx, request, command)
}

func (attempt *processStart) launchLocked(ctx context.Context, request processruntime.StartRequest, command *exec.Cmd) (processruntime.State, error) {
	if err := ctx.Err(); err != nil {
		return attempt.failLocked(err, 125)
	}
	// The start context governs preparation, not the lifetime of a launched
	// child. Attach disconnects must not kill a supervisor-owned process.
	process := attempt.process
	native, err := osprocess.Start(command, osprocess.Options{TTY: request.TTY, Output: processOutput{process: process}})
	if err != nil {
		exitCode := int32(125)
		if code, known := commandStartFailureExitCode(err); known {
			exitCode = code
		}
		return attempt.failLocked(fmt.Errorf("start native process: %w", err), exitCode)
	}
	process.native = native
	process.input = native.Input()
	if native.Terminal() {
		process.terminal = native
	}
	process.mu.Lock()
	process.state.Phase = phaseRunning
	process.state.PID = native.PID()
	started := cloneState(process.state)
	process.mu.Unlock()
	attempt.supervisor.processes[request.ID] = process
	go process.wait()
	return started, nil
}

func (attempt *processStart) failLocked(cause error, exitCode int32) (processruntime.State, error) {
	if attempt.process.transcript == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return processruntime.State{}, errors.Join(cause, attempt.rollbackLocked())
	}
	state, err := attempt.supervisor.recordCommandStartFailureLocked(attempt.process, cause, exitCode)
	if err != nil {
		return processruntime.State{}, errors.Join(cause, err, attempt.rollbackLocked())
	}
	// A recorded failure owns its identity just like a running command.
	return state, attempt.releaseResourcesLocked()
}

func (attempt *processStart) rollbackLocked() error {
	cleanupErr := attempt.releaseResourcesLocked()
	return errors.Join(cleanupErr, os.Remove(attempt.ownerPath))
}

func (attempt *processStart) releaseResourcesLocked() error {
	process := attempt.process
	attempt.supervisor.releaseCredentialAliasesLocked(process.processID, process.credentialLinks, process.agentCredentialLink)
	var closeErr error
	if process.transcript != nil {
		closeErr = process.transcript.Close()
		process.transcript = nil
	}
	return errors.Join(closeErr, os.RemoveAll(process.runtimeDir))
}

type processCredentials struct {
	root           string
	sshConfigLinks []string
	agentHome      string
}

// prepare checks cancellation between dependent filesystem operations. Cleanup
// deliberately runs without the cancelled context so temporary credentials are
// still released before the caller receives an outcome.
func (attempt *processStart) prepare(ctx context.Context, request processruntime.StartRequest) (*exec.Cmd, error) {
	if err := attempt.openTranscript(ctx, request); err != nil {
		return nil, err
	}
	credentials, err := attempt.prepareCredentials(ctx, request)
	if err != nil {
		return nil, err
	}
	return attempt.prepareCommand(ctx, request, credentials)
}

func (attempt *processStart) openTranscript(ctx context.Context, request processruntime.StartRequest) error {
	supervisor, process := attempt.supervisor, attempt.process
	if err := ctx.Err(); err != nil {
		return err
	}
	runtimeDirectory := request.RuntimeDirectory
	if runtimeDirectory == "" {
		runtimeDirectory = filepath.Join(supervisor.runtimeRoot, "processes", request.ID)
	}
	if filepath.Base(runtimeDirectory) != request.ID {
		return errors.New("process runtime directory must end with the process ID")
	}
	process.runtimeDir = runtimeDirectory
	if err := os.MkdirAll(runtimeDirectory, 0o700); err != nil {
		return fmt.Errorf("create temporary process runtime directory: %w", err)
	}
	transcriptPath := request.TranscriptPath
	if transcriptPath == "" {
		transcriptPath = filepath.Join(filepath.Dir(attempt.ownerPath), "transcript.log")
	}
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o700); err != nil {
		return fmt.Errorf("create transcript directory: %w", err)
	}
	transcript, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open process transcript: %w", err)
	}
	process.transcript, process.transcriptPath = transcript, transcriptPath
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (attempt *processStart) prepareCredentials(ctx context.Context, request processruntime.StartRequest) (processCredentials, error) {
	supervisor, process := attempt.supervisor, attempt.process
	runtimeDirectory := process.runtimeDir
	if err := ctx.Err(); err != nil {
		return processCredentials{}, err
	}
	if err := writeCredentialFiles(runtimeDirectory, request.CredentialFiles); err != nil {
		return processCredentials{}, err
	}
	credentialsRoot := request.CredentialsRoot
	if credentialsRoot == "" && (request.ExposeCredentials || len(request.SSHConfigFragments) > 0) {
		credentialsRoot = filepath.Join(supervisor.runtimeRoot, "credentials")
	}
	if err := ctx.Err(); err != nil {
		return processCredentials{}, err
	}
	links, err := supervisor.prepareCredentialAliases(request.ID, runtimeDirectory, credentialsRoot, request.CredentialFiles)
	if err != nil {
		return processCredentials{}, err
	}
	process.credentialLinks = append(process.credentialLinks, links...)
	if err := ctx.Err(); err != nil {
		return processCredentials{}, err
	}
	links, err = supervisor.prepareCredentialMounts(request.ID, runtimeDirectory, request.CredentialMounts)
	if err != nil {
		return processCredentials{}, err
	}
	process.credentialLinks = append(process.credentialLinks, links...)
	sshConfigPath := request.SSHConfigPath
	if sshConfigPath == "" && len(request.SSHConfigFragments) > 0 && supervisor.homeDir != "" {
		sshConfigPath = filepath.Join(supervisor.homeDir, ".ssh", "config")
	}
	if err := ctx.Err(); err != nil {
		return processCredentials{}, err
	}
	sshConfigLinks, err := supervisor.prepareSSHConfig(request.ID, runtimeDirectory, credentialsRoot, sshConfigPath, request.SSHConfigFragments)
	if err != nil {
		return processCredentials{}, err
	}
	process.credentialLinks = append(process.credentialLinks, sshConfigLinks...)
	agentHome := request.AgentHome
	if agentHome == "" && request.Agent != nil && request.Agent.Type != "" && supervisor.homeDir != "" {
		credential := request.Agent.Credential
		if credential == "" {
			credential = "default"
		}
		agentHome = filepath.Join(supervisor.homeDir, ".rc", "agents", request.Agent.Type, credential)
	}
	if err := ctx.Err(); err != nil {
		return processCredentials{}, err
	}
	agentLink, err := supervisor.prepareAgentHome(request.ID, runtimeDirectory, agentHome, request.CredentialFiles)
	if err != nil {
		return processCredentials{}, err
	}
	process.agentCredentialLink = agentLink
	return processCredentials{root: credentialsRoot, sshConfigLinks: sshConfigLinks, agentHome: agentHome}, nil
}

func (attempt *processStart) prepareCommand(ctx context.Context, request processruntime.StartRequest, credentials processCredentials) (*exec.Cmd, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	supervisor, process := attempt.supervisor, attempt.process
	runtimeDirectory, credentialsRoot := process.runtimeDir, credentials.root
	sshConfigLinks, agentHome := credentials.sshConfigLinks, credentials.agentHome
	command := exec.Command(request.Command[0], request.Command[1:]...)
	command.WaitDelay = supervisor.stopGrace
	command.Dir = request.WorkingDirectory
	if command.Dir == "" {
		switch request.DefaultDirectory {
		case processruntime.DefaultDirectoryHome:
			command.Dir = supervisor.homeDir
		case processruntime.DefaultDirectoryWorkspace:
			command.Dir = supervisor.workspaceDir
		}
	}
	processEnvironment := maps.Clone(request.Environment)
	if processEnvironment == nil {
		processEnvironment = make(map[string]string)
	}
	if runtime.GOOS == runtimeOSWindows {
		if err := configureWindowsGitSSH(processEnvironment, runtimeDirectory, sshConfigLinks); err != nil {
			return nil, err
		}
	}
	if agentHome != "" {
		if _, exists := processEnvironment["RC_AGENT_HOME"]; !exists {
			processEnvironment["RC_AGENT_HOME"] = agentHome
		}
		if request.Agent != nil && request.Agent.Type == "codex" {
			if _, exists := processEnvironment["CODEX_HOME"]; !exists {
				processEnvironment["CODEX_HOME"] = agentHome
			}
		}
	}
	if request.ExposeCredentials {
		if _, exists := processEnvironment["RC_CREDENTIALS_DIR"]; !exists {
			processEnvironment["RC_CREDENTIALS_DIR"] = credentialsRoot
		}
	}
	if request.TTY {
		if _, configured := processEnvironment["TERM"]; !configured {
			processEnvironment["TERM"] = "xterm-256color"
		}
	}
	commandEnvironment, err := mergedEnvironment(processEnvironment)
	if err != nil {
		return nil, err
	}
	command.Env = commandEnvironment
	transcriptInfo, err := process.transcript.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat process transcript: %w", err)
	}
	process.transcriptLen = transcriptInfo.Size()
	return command, nil
}
