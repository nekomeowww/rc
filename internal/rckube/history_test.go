//go:build !windows

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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	processruntime "github.com/nekomeowww/rc/internal/execution"
)

const testHistoryTimeout = 3 * time.Second

func waitForExit(t *testing.T, supervisor *Supervisor, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		state, err := supervisor.Inspect(id)
		return err == nil && state.Phase == phaseExited
	}, testHistoryTimeout, 10*time.Millisecond, "wait for %s to exit", id)
}

func TestSupervisorRecordsFinishedProcessAcrossRestart(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	request := processruntime.StartRequest{
		ID:               "recorded",
		UID:              testProcessUID,
		Command:          []string{"sh", "-c", "exit 3"},
		WorkingDirectory: t.TempDir(),
		Environment:      map[string]string{"SECRET_TOKEN": "never-recorded"},
	}
	first := NewSupervisor(stateDirectory, 100*time.Millisecond)
	_, err := first.Start(t.Context(), request)
	requirements.NoError(err)
	waitForExit(t, first, request.ID)

	data, err := os.ReadFile(filepath.Join(stateDirectory, request.ID, recordFileName))
	requirements.NoError(err, "write record.json next to the transcript")
	assertions.NotContains(string(data), "never-recorded", "never record environment values")

	second := NewSupervisor(stateDirectory, 100*time.Millisecond)
	requirements.NoError(second.RecoverHistory(time.Now()))
	state, err := second.Inspect(request.ID)
	requirements.NoError(err, "report a finished process after a supervisor restart")
	assertions.Equal(phaseExited, state.Phase)
	requirements.NotNil(state.ExitCode)
	assertions.Equal(int32(3), *state.ExitCode, "keep the real exit code instead of Lost")

	entries, err := second.List()
	requirements.NoError(err)
	requirements.Len(entries, 1)
	entry := entries[0]
	assertions.Equal(request.Command, entry.Command)
	assertions.Equal(request.WorkingDirectory, entry.WorkingDirectory)
	requirements.NotNil(entry.StartedAt)
	requirements.NotNil(entry.CompletedAt)
	assertions.False(entry.CompletedAt.Before(*entry.StartedAt))

	_, err = second.Start(t.Context(), request)
	requirements.ErrorIs(err, processruntime.ErrNotFound, "a recorded identity still never runs twice")
}

func TestSupervisorRecordsStartFailure(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	supervisor := NewSupervisor(stateDirectory, 100*time.Millisecond)
	request := processruntime.StartRequest{ID: "missing", UID: testProcessUID, Command: []string{"/definitely/missing"}}
	_, err := supervisor.Start(t.Context(), request)
	requirements.NoError(err)

	entries, err := NewSupervisor(stateDirectory, time.Second).List()
	requirements.NoError(err)
	requirements.Len(entries, 1)
	requirements.Equal(phaseExited, entries[0].Phase)
	requirements.NotNil(entries[0].ExitCode)
	requirements.Equal(int32(127), *entries[0].ExitCode)
	requirements.NotNil(entries[0].CompletedAt)
}

func TestRecoverHistoryMarksUnfinishedRecordsLost(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	request := processruntime.StartRequest{ID: "orphaned", UID: testProcessUID, Command: []string{"sleep", "30"}}
	first := NewSupervisor(stateDirectory, 100*time.Millisecond)
	_, err := first.Start(t.Context(), request)
	requirements.NoError(err)
	t.Cleanup(func() { _, _ = first.Stop(request.ID) })

	second := NewSupervisor(stateDirectory, 100*time.Millisecond)
	recovered := time.Now()
	requirements.NoError(second.RecoverHistory(recovered))
	entries, err := second.List()
	requirements.NoError(err)
	requirements.Len(entries, 1)
	assertions.Equal(phaseLost, entries[0].Phase)
	assertions.Equal(reasonSupervisorRestarted, entries[0].Reason)
	assertions.Zero(entries[0].PID)
	requirements.NotNil(entries[0].CompletedAt)
	assertions.WithinDuration(recovered, *entries[0].CompletedAt, time.Second)
	_, err = second.Inspect(request.ID)
	assertions.ErrorIs(err, processruntime.ErrNotFound, "the controller still marks a lost process Lost")
}

func TestListOrdersNewestStartFirstWithLiveState(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	supervisor := NewSupervisor(stateDirectory, 100*time.Millisecond)
	older := processruntime.StartRequest{ID: "older", UID: testProcessUID, Command: []string{testTrueCommand}}
	_, err := supervisor.Start(t.Context(), older)
	requirements.NoError(err)
	waitForExit(t, supervisor, older.ID)
	newer := processruntime.StartRequest{ID: "newer", UID: testProcessUID, Command: []string{"sleep", "30"}}
	_, err = supervisor.Start(t.Context(), newer)
	requirements.NoError(err)
	t.Cleanup(func() { _, _ = supervisor.Stop(newer.ID) })

	entries, err := supervisor.List()
	requirements.NoError(err)
	requirements.Len(entries, 2)
	requirements.Equal(newer.ID, entries[0].ID)
	requirements.Equal(phaseRunning, entries[0].Phase, "report live state for an owned process")
	requirements.Equal(older.ID, entries[1].ID)
}

func TestPruneHistoryAppliesAgeCountAndTranscriptBounds(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	write := func(id, phase string, completedAgo time.Duration) {
		t.Helper()
		requirements.NoError(os.MkdirAll(filepath.Join(stateDirectory, id), 0o700))
		requirements.NoError(os.WriteFile(filepath.Join(stateDirectory, id, "owner.uid"), []byte(testProcessUID), 0o600))
		requirements.NoError(os.WriteFile(filepath.Join(stateDirectory, id, "transcript.log"), []byte(id), 0o600))
		entry := processruntime.HistoryEntry{State: processruntime.State{ID: id, UID: testProcessUID, Phase: phase}, Command: []string{testTrueCommand}}
		started := now.Add(-completedAgo - time.Minute)
		entry.StartedAt = &started
		if historyTerminal(phase) {
			completed := now.Add(-completedAgo)
			entry.CompletedAt = &completed
		}
		requirements.NoError(writeRecord(stateDirectory, entry))
	}
	write("expired", phaseExited, 91*day)
	write("overflow", phaseStopped, 30*day)
	write("old-transcript", phaseLost, 20*day)
	write("recent", phaseExited, day)
	write("active", phaseRunning, 200*day)
	requirements.NoError(os.MkdirAll(filepath.Join(stateDirectory, "legacy"), 0o700))
	requirements.NoError(os.WriteFile(filepath.Join(stateDirectory, "legacy", "owner.uid"), []byte(testProcessUID), 0o600))

	supervisor := NewSupervisor(stateDirectory, time.Second)
	removed, err := supervisor.PruneHistory(processruntime.HistoryPolicy{
		TTLAfterFinished: 90 * day,
		MaxEntries:       2,
		TranscriptTTL:    14 * day,
	}, now)
	requirements.NoError(err)
	assertions.Equal(2, removed)

	assertions.NoDirExists(filepath.Join(stateDirectory, "expired"), "remove a record past ttlAfterFinished")
	assertions.NoDirExists(filepath.Join(stateDirectory, "overflow"), "remove the oldest record beyond maxEntries")
	assertions.FileExists(filepath.Join(stateDirectory, "old-transcript", recordFileName), "keep a record within both bounds")
	assertions.NoFileExists(filepath.Join(stateDirectory, "old-transcript", "transcript.log"), "remove a transcript past transcriptTTL")
	assertions.FileExists(filepath.Join(stateDirectory, "recent", "transcript.log"))
	assertions.FileExists(filepath.Join(stateDirectory, "active", "owner.uid"), "never prune an unfinished record")
	assertions.FileExists(filepath.Join(stateDirectory, "legacy", "owner.uid"), "skip a legacy directory without a record")

	removed, err = supervisor.PruneHistory(processruntime.HistoryPolicy{}, now)
	requirements.NoError(err)
	assertions.Zero(removed, "a zero policy disables retention")
}

func TestPrepareTranscriptScopeRemovesInheritedRecords(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	stateDirectory := t.TempDir()
	requirements.NoError(PrepareTranscriptScope(stateDirectory, "source-uid"))
	requirements.NoError(os.MkdirAll(filepath.Join(stateDirectory, "copied"), 0o700))
	requirements.NoError(writeRecord(stateDirectory, processruntime.HistoryEntry{State: processruntime.State{ID: "copied", Phase: phaseExited}}))

	requirements.NoError(PrepareTranscriptScope(stateDirectory, "clone-uid"))
	requirements.NoFileExists(filepath.Join(stateDirectory, "copied", recordFileName), "a clone does not inherit another target's history")
}
