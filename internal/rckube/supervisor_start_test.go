package rckube

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
)

const missingStartCommand = "command-must-not-run"

func TestSupervisorRetainsSSHPreparationFailure(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	runtimeRoot := t.TempDir()
	supervisor := NewSupervisor(stateDirectory, time.Second, WithRoots(t.TempDir(), t.TempDir(), runtimeRoot))
	request := processruntime.StartRequest{
		ID: "ssh-rejected", UID: "ssh-uid", Command: []string{missingStartCommand},
		ExposeCredentials: true, SSHConfigPath: "relative-config",
		CredentialFiles:    map[string][]byte{"credentials/selected/id": []byte("synthetic-key")},
		SSHConfigFragments: map[string]string{"selected": "Host github.com\n  IdentityFile ${identityFile}\n"},
	}
	failed, err := supervisor.Start(t.Context(), request)
	requirements.NoError(err, "return an observable terminal outcome when SSH preparation fails")
	assertions.Equal(phaseExited, failed.Phase)
	requirements.NotNil(failed.ExitCode)
	assertions.Equal(int32(125), *failed.ExitCode, "distinguish runtime preparation from a Git exit code")
	assertions.Contains(failed.Reason, "SSH config path")
	assertions.Zero(failed.PID, "Git was never launched")
	assertions.NoDirExists(filepath.Join(runtimeRoot, "processes", request.ID))
	assertions.NoFileExists(filepath.Join(runtimeRoot, "credentials", "selected", "id"))

	request.SSHConfigPath = filepath.Join(t.TempDir(), "config")
	retried, err := supervisor.Start(t.Context(), request)
	requirements.NoError(err)
	assertions.Equal(failed, retried, "a duplicate request must not launch after a terminal outcome")
	inspected, err := supervisor.Inspect(request.ID)
	requirements.NoError(err)
	assertions.Equal(failed, inspected)

	restarted := NewSupervisor(stateDirectory, time.Second)
	_, err = restarted.Start(t.Context(), request)
	requirements.ErrorIs(err, processruntime.ErrNotFound, "retain ownership across supervisor restarts")
}

func TestCancelledStartDoesNotClaimIdentity(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	directory := t.TempDir()
	supervisor := NewSupervisor(directory, time.Second)
	request := processruntime.StartRequest{ID: "cancelled", UID: "cancelled-uid", Command: []string{missingStartCommand}}
	_, err := supervisor.Start(ctx, request)
	require.ErrorIs(t, err, context.Canceled)
	assert.NoDirExists(t, filepath.Join(directory, request.ID))
	_, err = supervisor.Inspect(request.ID)
	require.ErrorIs(t, err, processruntime.ErrNotFound)
	state, err := supervisor.Start(t.Context(), request)
	require.NoError(t, err, "a cancelled unstarted request does not consume the identity")
	assert.Equal(t, phaseExited, state.Phase)
}

func TestCancellationAfterPreparationRollsBackResources(t *testing.T) {
	t.Parallel()
	directory, runtimeRoot := t.TempDir(), t.TempDir()
	supervisor := NewSupervisor(directory, time.Second, WithRoots(t.TempDir(), t.TempDir(), runtimeRoot))
	request := processruntime.StartRequest{
		ID: "prepared", UID: "prepared-uid", Command: []string{missingStartCommand},
		ExposeCredentials:  true,
		CredentialFiles:    map[string][]byte{"credentials/selected/id": []byte("synthetic-key")},
		SSHConfigFragments: map[string]string{"selected": "Host test.invalid\n  IdentityFile ${identityFile}\n"},
	}
	ownerPath := filepath.Join(directory, "owner.uid")
	require.NoError(t, os.WriteFile(ownerPath, []byte(request.UID), 0o600))
	attempt := &processStart{supervisor: supervisor, ownerPath: ownerPath, process: &supervisedProcess{
		state: processruntime.State{ID: request.ID, UID: request.UID}, processID: request.ID,
	}}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command, err := attempt.prepare(ctx, request)
	require.NoError(t, err)
	credentialPath := filepath.Join(runtimeRoot, "credentials", "selected", "id")
	require.FileExists(t, credentialPath)
	cancel()
	_, err = attempt.launchLocked(ctx, request, command)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, attempt.process.native)
	assert.Nil(t, attempt.process.transcript)
	assert.NoFileExists(t, ownerPath)
	assert.NoDirExists(t, attempt.process.runtimeDir)
	_, err = os.Lstat(filepath.Dir(credentialPath))
	assert.ErrorIs(t, err, os.ErrNotExist, "release the credential alias as well as its contents")
	assert.Empty(t, supervisor.processes)
	assert.Empty(t, supervisor.aliases)
}

func TestStartRollbackCollectsCleanupErrors(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	transcript, err := os.CreateTemp(directory, "transcript")
	require.NoError(t, err)
	require.NoError(t, transcript.Close())
	ownerPath := filepath.Join(directory, "occupied-owner")
	require.NoError(t, os.Mkdir(ownerPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ownerPath, "keep"), nil, 0o600))
	removeErr := os.Remove(ownerPath)
	require.Error(t, removeErr)
	attempt := &processStart{supervisor: NewSupervisor(directory, time.Second), ownerPath: ownerPath,
		process: &supervisedProcess{transcript: transcript, processID: "rollback"},
	}
	_, err = attempt.failLocked(context.Canceled, 125)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, err, os.ErrClosed, "preserve transcript cleanup failure")
	assert.ErrorIs(t, err, errors.Unwrap(removeErr), "attempt identity cleanup even when close fails")
}
