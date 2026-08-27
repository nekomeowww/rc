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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
)

const (
	testProcessUID          = "uid"
	testTrueCommand         = "true"
	testCredentialsVariable = "RC_CREDENTIALS_DIR"
	testCredentialDataPath  = "credentials/custom/data"
)

func TestSupervisorRunsCommandOnceAndPersistsTranscript(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	supervisor := NewSupervisor(stateDirectory, 100*time.Millisecond)
	request := processruntime.StartRequest{
		ID: "process-01", UID: "uid-01", Command: []string{"sh", "-c", "printf 'supervised output'; exit 7"},
		WorkingDirectory: t.TempDir(), TranscriptPath: filepath.Join(stateDirectory, "process-01", "transcript.log"),
	}

	started, err := supervisor.Start(request)
	requirements.NoError(err, "start command")
	assertions.Equal(phaseRunning, started.Phase, "return after supervisor owns process")
	assertions.Positive(started.PID, "record child PID")

	requirements.Eventually(func() bool {
		state, inspectErr := supervisor.Inspect(request.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "observe command exit")
	finished, err := supervisor.Inspect(request.ID)
	requirements.NoError(err, "inspect exited process")
	requirements.NotNil(finished.ExitCode, "record exit code")
	assertions.Equal(int32(7), *finished.ExitCode, "preserve child exit code")
	owned, err := supervisor.process(request.ID)
	requirements.NoError(err, "get retained process state")
	owned.mu.Lock()
	assertions.Nil(owned.transcript, "close transcript file after process exit")
	owned.mu.Unlock()

	duplicate, err := supervisor.Start(request)
	requirements.NoError(err, "idempotent duplicate start")
	assertions.Equal(finished.PID, duplicate.PID, "return original process instead of rerunning")
	assertions.Equal(finished.ExitCode, duplicate.ExitCode, "return original terminal state")

	var transcript bytes.Buffer
	requirements.NoError(supervisor.Logs(request.ID, &transcript), "read persistent transcript")
	assertions.Equal("supervised output", transcript.String(), "persist exact command output")
}

func TestSupervisorRejectsProcessIDReusedWithAnotherUID(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	request := processruntime.StartRequest{ID: "same-id", UID: "first-uid", Command: []string{"sh", "-c", "exit 0"}}
	_, err := supervisor.Start(request)
	requirements.NoError(err, "start original process")

	request.UID = "different-uid"
	_, err = supervisor.Start(request)
	requirements.ErrorIs(err, ErrProcessConflict, "process ID cannot identify another CR UID")
}

func TestSupervisorRejectsProcessIDThatEscapesStateDirectory(t *testing.T) {
	t.Parallel()
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	_, err := supervisor.Start(processruntime.StartRequest{ID: "../escape", UID: testProcessUID, Command: []string{testTrueCommand}})
	require.EqualError(t, err, "process ID must be one safe path segment")
}

func TestSupervisorDoesNotConsumeIdentityWhenRequestFailsBeforeLaunch(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	request := processruntime.StartRequest{
		ID: "retryable", UID: testProcessUID, Command: []string{testTrueCommand},
		RuntimeDirectory: filepath.Join(t.TempDir(), "wrong-process-id"),
	}
	_, err := supervisor.Start(request)
	requirements.Error(err, "report pre-launch failure")
	request.RuntimeDirectory = filepath.Join(t.TempDir(), request.ID)
	_, err = supervisor.Start(request)
	requirements.NoError(err, "retry same identity after no child was launched")
}

func TestSupervisorRecordsMissingExecutableAsTerminalCommandFailure(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	supervisor := NewSupervisor(stateDirectory, 100*time.Millisecond)
	request := processruntime.StartRequest{
		ID: "missing-command", UID: testProcessUID, Command: []string{"/definitely/missing/command"},
		TranscriptPath: filepath.Join(stateDirectory, "missing-command", "transcript.log"),
	}

	failed, err := supervisor.Start(request)
	requirements.NoError(err, "a missing executable is a terminal command outcome")
	requirements.NotNil(failed.ExitCode, "publish a conventional command-not-found exit status")
	assertions.Equal(phaseExited, failed.Phase, "retain an inspectable terminal process")
	assertions.Equal(int32(127), *failed.ExitCode, "use the conventional command-not-found exit status")
	assertions.Contains(failed.Reason, "no such file", "preserve the start failure reason")

	duplicate, err := supervisor.Start(request)
	requirements.NoError(err, "duplicate start returns the retained command failure")
	assertions.Equal(failed, duplicate, "do not retry a terminal command failure")
	var transcript bytes.Buffer
	requirements.NoError(supervisor.Logs(request.ID, &transcript), "read failed command transcript")
	assertions.Contains(transcript.String(), "could not start", "make the failure visible through AgentProcess logs")
}

func TestSupervisorBoundsPersistentTranscript(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond, WithMaxTranscriptBytes(16))
	request := processruntime.StartRequest{ID: "bounded", UID: testProcessUID, Command: []string{"sh", "-c", "printf 12345678901234567890"}}
	_, err := supervisor.Start(request)
	requirements.NoError(err, "start output producer")
	requirements.Eventually(func() bool {
		state, inspectErr := supervisor.Inspect(request.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "wait for producer")
	var transcript bytes.Buffer
	requirements.NoError(supervisor.Logs(request.ID, &transcript), "read bounded transcript")
	requirements.Contains(transcript.String(), "[rc-kube transcript truncated]", "report truncation in durable output")
}

func TestSupervisorKeepsCredentialsOutOfPersistentState(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	runtimeDirectory := filepath.Join(t.TempDir(), "credential-scope")
	credentialsRoot := t.TempDir()
	supervisor := NewSupervisor(stateDirectory, 100*time.Millisecond)
	request := processruntime.StartRequest{
		ID: "credential-scope", UID: testProcessUID, Command: []string{"sh", "-c", "cat \"$RC_CREDENTIALS_DIR/github/token\""},
		Environment: map[string]string{testCredentialsVariable: credentialsRoot}, RuntimeDirectory: runtimeDirectory,
		CredentialsRoot: credentialsRoot, CredentialFiles: map[string][]byte{"credentials/github/token": []byte("secret")},
	}
	_, err := supervisor.Start(request)
	requirements.NoError(err, "start credential consumer")
	requirements.Eventually(func() bool {
		state, inspectErr := supervisor.Inspect(request.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "wait for credential consumer")
	var transcript bytes.Buffer
	requirements.NoError(supervisor.Logs(request.ID, &transcript), "read consumer output")
	requirements.Equal("secret", transcript.String(), "expose the standard credentials directory")
	_, err = os.Stat(filepath.Join(stateDirectory, request.ID, "credentials"))
	requirements.ErrorIs(err, os.ErrNotExist, "persistent process state never stores credential files")
	_, err = os.Stat(runtimeDirectory)
	requirements.ErrorIs(err, os.ErrNotExist, "remove temporary process credentials after exit")
	_, err = os.Lstat(filepath.Join(credentialsRoot, "github"))
	requirements.ErrorIs(err, os.ErrNotExist, "remove generic credential alias after exit")
}

func TestSupervisorProjectsSSHConfigAndPreservesUserConfiguration(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	runtimeRoot := t.TempDir()
	runtimeDirectory := filepath.Join(runtimeRoot, "ssh-config")
	credentialsRoot := filepath.Join(runtimeRoot, "credentials")
	sshDirectory := filepath.Join(t.TempDir(), ".ssh")
	sshConfigPath := filepath.Join(sshDirectory, "config")
	requirements.NoError(os.MkdirAll(sshDirectory, 0o700), "create SSH directory")
	requirements.NoError(os.WriteFile(sshConfigPath, []byte("Host existing\n  User existing\n"), 0o600), "create user SSH config")
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	request := processruntime.StartRequest{
		ID: "ssh-config", UID: testProcessUID,
		Command: []string{"sh", "-c", "cat \"$SSH_CONFIG\"; cat \"$SSH_FRAGMENT\"; cat \"$RC_CREDENTIALS_DIR/github/id\""},
		Environment: map[string]string{
			testCredentialsVariable: credentialsRoot,
			"SSH_CONFIG":            sshConfigPath,
			"SSH_FRAGMENT":          sshConfigPath + ".d/rc-github.conf",
		},
		RuntimeDirectory: runtimeDirectory,
		CredentialsRoot:  credentialsRoot,
		CredentialFiles: map[string][]byte{
			"credentials/github/id":          []byte("private-key"),
			"credentials/github/known_hosts": []byte("github.com ssh-ed25519 host-key"),
		},
		SSHConfigPath: sshConfigPath,
		SSHConfigFragments: map[string]string{
			"github": "Host github.com\n  IdentityFile ${identityFile}\n  UserKnownHostsFile ${knownHostsFile}\n",
		},
	}

	_, err := supervisor.Start(request)
	requirements.NoError(err, "start SSH credential consumer")
	requirements.Eventually(func() bool {
		state, inspectErr := supervisor.Inspect(request.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "wait for SSH credential consumer")
	var transcript bytes.Buffer
	requirements.NoError(supervisor.Logs(request.ID, &transcript), "read SSH credential consumer output")
	requirements.Contains(transcript.String(), "Host existing\n  User existing\n", "preserve user SSH configuration")
	requirements.Contains(transcript.String(), "Host *\n  Include "+sshConfigPath+".d/*.conf\n", "load rc-managed SSH fragments")
	requirements.Contains(transcript.String(), "IdentityFile "+credentialsRoot+"/github/id", "render identity file placeholder")
	requirements.Contains(transcript.String(), "UserKnownHostsFile "+credentialsRoot+"/github/known_hosts", "render known hosts placeholder")
	requirements.Contains(transcript.String(), "private-key", "keep identity available while process runs")

	requirements.NoError(ensureSSHConfigInclude(sshConfigPath, sshConfigPath+".d/*.conf"), "reconcile managed include idempotently")
	persistedConfig, err := os.ReadFile(sshConfigPath)
	requirements.NoError(err, "read persistent SSH config")
	requirements.Equal("Host existing\n  User existing\n# rc: managed SSH Credential fragments\nHost *\n  Include "+sshConfigPath+".d/*.conf\n", string(persistedConfig), "retain one managed include")
	_, err = os.Lstat(sshConfigPath + ".d/rc-github.conf")
	requirements.ErrorIs(err, os.ErrNotExist, "remove SSH config fragment after process exit")
	_, err = os.Lstat(filepath.Join(credentialsRoot, "github"))
	requirements.ErrorIs(err, os.ErrNotExist, "remove SSH credential files after process exit")
}

func TestSupervisorProjectsWritableCredentialFileForProcessLifetime(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	runtimeRoot := t.TempDir()
	runtimeDirectory := filepath.Join(runtimeRoot, "credential-consumer")
	mountPath := filepath.Join(t.TempDir(), ".tool", "credentials.json")
	supervisor := NewSupervisor(stateDirectory, 100*time.Millisecond)
	request := processruntime.StartRequest{
		ID: "credential-consumer", UID: testProcessUID,
		Command:     []string{"sh", "-c", "cat \"$CREDENTIAL_FILE\"; printf rotated > \"$CREDENTIAL_FILE\""},
		Environment: map[string]string{"CREDENTIAL_FILE": mountPath}, RuntimeDirectory: runtimeDirectory,
		CredentialFiles:  map[string][]byte{testCredentialDataPath: []byte("portable")},
		CredentialMounts: []processruntime.CredentialMount{{Source: testCredentialDataPath, Target: mountPath}},
	}

	_, err := supervisor.Start(request)
	requirements.NoError(err, "start native credential consumer")
	requirements.Eventually(func() bool {
		state, inspectErr := supervisor.Inspect(request.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "wait for native credential consumer")
	var transcript bytes.Buffer
	requirements.NoError(supervisor.Logs(request.ID, &transcript), "read native credential consumer output")
	requirements.Equal("portable", transcript.String(), "read projected native credential file")
	_, err = os.Lstat(mountPath)
	requirements.ErrorIs(err, os.ErrNotExist, "remove native credential link after exit")
	_, err = os.Stat(runtimeDirectory)
	requirements.ErrorIs(err, os.ErrNotExist, "remove temporary process credential data after exit")
}

func TestSupervisorDoesNotReplaceExistingCredentialMountTarget(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	mountPath := filepath.Join(t.TempDir(), "credentials.json")
	requirements.NoError(os.WriteFile(mountPath, []byte("user-data"), 0o600), "create existing target")
	runtimeDirectory := filepath.Join(t.TempDir(), "credential-conflict")
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	request := processruntime.StartRequest{
		ID: "credential-conflict", UID: testProcessUID, Command: []string{testTrueCommand}, RuntimeDirectory: runtimeDirectory,
		CredentialFiles:  map[string][]byte{testCredentialDataPath: []byte("credential")},
		CredentialMounts: []processruntime.CredentialMount{{Source: testCredentialDataPath, Target: mountPath}},
	}

	_, err := supervisor.Start(request)
	requirements.Error(err, "reject a mount over an existing regular file")
	data, err := os.ReadFile(mountPath)
	requirements.NoError(err, "read preserved target")
	requirements.Equal([]byte("user-data"), data, "preserve an existing user file")
	_, err = os.Stat(runtimeDirectory)
	requirements.ErrorIs(err, os.ErrNotExist, "clean temporary credential data after rejection")
}

func TestSharedCredentialProjectionSurvivesAnotherProcessExit(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	runtimeRoot := t.TempDir()
	credentialsRoot := filepath.Join(runtimeRoot, "credentials")
	supervisor := NewSupervisor(stateDirectory, 100*time.Millisecond)
	credentialFiles := map[string][]byte{"credentials/github/token": []byte("secret")}
	first := processruntime.StartRequest{
		ID: "first", UID: "first-uid", Command: []string{"sh", "-c", "sleep 0.4; cat \"$RC_CREDENTIALS_DIR/github/token\""},
		Environment: map[string]string{testCredentialsVariable: credentialsRoot}, RuntimeDirectory: filepath.Join(runtimeRoot, "first"),
		CredentialsRoot: credentialsRoot, CredentialFiles: credentialFiles,
	}
	second := processruntime.StartRequest{
		ID: "second", UID: "second-uid", Command: []string{testTrueCommand},
		Environment: map[string]string{testCredentialsVariable: credentialsRoot}, RuntimeDirectory: filepath.Join(runtimeRoot, "second"),
		CredentialsRoot: credentialsRoot, CredentialFiles: credentialFiles,
	}
	_, err := supervisor.Start(first)
	requirements.NoError(err, "start first credential consumer")
	_, err = supervisor.Start(second)
	requirements.NoError(err, "start second credential consumer")
	requirements.Eventually(func() bool {
		state, inspectErr := supervisor.Inspect(second.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "wait for second process")
	_, err = os.Stat(filepath.Join(credentialsRoot, "github", "token"))
	requirements.NoError(err, "retain shared projection while first process is active")
	requirements.Eventually(func() bool {
		state, inspectErr := supervisor.Inspect(first.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "wait for first process")
	var transcript bytes.Buffer
	requirements.NoError(supervisor.Logs(first.ID, &transcript), "read first process output")
	requirements.Equal("secret", transcript.String(), "first process retains credential after second exits")
}

func TestSupervisorRestartNeverRerunsPersistedProcessIdentity(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	request := processruntime.StartRequest{ID: "restart-safe", UID: testProcessUID, Command: []string{testTrueCommand}}
	first := NewSupervisor(stateDirectory, 100*time.Millisecond)
	_, err := first.Start(request)
	requirements.NoError(err, "start original process")
	requirements.Eventually(func() bool {
		state, inspectErr := first.Inspect(request.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "wait for original process")
	second := NewSupervisor(stateDirectory, 100*time.Millisecond)
	_, err = second.Start(request)
	requirements.ErrorIs(err, processruntime.ErrNotFound, "persisted UID becomes lost instead of running twice")
}

func TestProcessOutputDisconnectsSlowClientInsteadOfDroppingBytes(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	transcript, err := os.CreateTemp(t.TempDir(), "transcript-")
	requirements.NoError(err, "create transcript")
	t.Cleanup(func() { _ = transcript.Close() })
	stream := make(chan []byte, 1)
	stream <- []byte("pending")
	process := &supervisedProcess{transcript: transcript, clients: map[string]chan []byte{"slow": stream}}

	_, err = (processOutput{process: process}).Write([]byte("new"))
	requirements.NoError(err, "write process output")
	requirements.Empty(process.clients, "remove lagging client")
	<-stream
	_, open := <-stream
	requirements.False(open, "close lagging client stream explicitly")
}

func TestRecentInputControlsSharedTerminalViewport(t *testing.T) {
	t.Parallel()
	process := &supervisedProcess{terminal: os.Stdin, foregroundClient: "first"}
	supervisor := &Supervisor{processes: map[string]*supervisedProcess{"process": process}}
	require.NoError(t, supervisor.Resize("process", "second", 24, 80), "remember background client viewport")
	require.Equal(t, "first", process.foregroundClient, "background resize does not steal foreground")

	var input bytes.Buffer
	_, err := (foregroundWriter{process: process, clientID: "second", target: &input}).Write([]byte("x"))
	require.NoError(t, err, "write client input")
	require.Equal(t, "second", process.foregroundClient, "most recent interaction becomes foreground")
	require.NoError(t, supervisor.Resize("process", "first", 24, 80), "remember previous client viewport without stealing foreground")
	require.Equal(t, "second", process.foregroundClient, "resize alone does not change foreground")
}

func TestTerminalAttachStreamsRawPTYBytes(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	request := processruntime.StartRequest{
		ID: "raw-terminal", UID: testProcessUID,
		Command: []string{"sh", "-c", "sleep 0.1; printf '\\033[31mraw-color\\033[0m'"}, TTY: true,
	}
	_, err := supervisor.Start(request)
	requirements.NoError(err, "start terminal process")
	var attached bytes.Buffer
	requirements.NoError(supervisor.Attach(context.Background(), request.ID, "viewer", nil, &attached, 24, 80), "attach to live terminal stream")
	requirements.Contains(attached.String(), "\x1b[31mraw-color\x1b[0m", "preserve the child PTY ANSI sequence byte-for-byte")
}

func TestTerminalAttachAppliesInitialSizeBeforeForwardingInput(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	request := processruntime.StartRequest{
		ID: "initial-size", UID: testProcessUID,
		Command: []string{"sh", "-c", "read line; stty size"}, TTY: true,
	}
	_, err := supervisor.Start(request)
	requirements.NoError(err, "start terminal process")
	var attached bytes.Buffer
	input := bytes.NewBufferString("continue\n")
	requirements.NoError(supervisor.Attach(context.Background(), request.ID, "viewer", input, &attached, 42, 120), "attach with local terminal size")
	requirements.Contains(attached.String(), "42 120", "resize the child PTY before forwarding attached input")
}
