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

func TestTerminalAttachRendersCanonicalScreenInsteadOfRawTranscript(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	request := processruntime.StartRequest{ID: "screen", UID: testProcessUID, Command: []string{"sh", "-c", "printf 'first\\rsecond'"}, TTY: true}
	_, err := supervisor.Start(request)
	requirements.NoError(err, "start terminal process")
	requirements.Eventually(func() bool {
		state, inspectErr := supervisor.Inspect(request.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "wait for terminal process")
	var rendered bytes.Buffer
	requirements.NoError(supervisor.Attach(context.Background(), request.ID, "viewer", nil, &rendered), "render terminal screen")
	requirements.Contains(rendered.String(), "second", "render final screen content")
	requirements.NotContains(rendered.String(), "first\rsecond", "do not replay raw control stream")
}
