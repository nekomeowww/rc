package rckube

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/stretchr/testify/require"
)

func TestTranscriptCleanupKeepsAtMostOnceAcrossRestart(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	id, uid := "completed", "completed-uid"
	require.NoError(t, os.MkdirAll(filepath.Join(directory, id), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, id, "owner.uid"), []byte(uid), 0o600))
	transcript := filepath.Join(directory, id, "transcript.log")
	require.NoError(t, os.WriteFile(transcript, []byte("private output"), 0o600))
	supervisor := NewSupervisor(directory, time.Second)
	supervisor.processes[id] = &supervisedProcess{state: processruntime.State{UID: uid, Phase: phaseExited}}
	require.ErrorIs(t, supervisor.PruneTranscript(id, "wrong-uid"), ErrProcessConflict)
	require.FileExists(t, transcript)
	require.NoError(t, supervisor.PruneTranscript(id, uid))
	require.NoFileExists(t, transcript)
	require.Empty(t, supervisor.processes, "release completed process memory")
	// ROOT CAUSE: removing owner.uid with the transcript would allow a replayed
	// Start to launch again after CR deletion or a supervisor restart.
	supervisor = NewSupervisor(directory, time.Second)
	require.NoError(t, supervisor.PruneTranscript(id, uid), "idempotent after restart")
	_, err := supervisor.Start(context.Background(), processruntime.StartRequest{ID: id, UID: uid, Command: []string{"must-never-run"}})
	require.ErrorIs(t, err, processruntime.ErrNotFound)
	require.FileExists(t, filepath.Join(directory, id, "owner.uid"))
}

func TestTranscriptCleanupRejectsActiveProcesses(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{phaseStarting, phaseRunning} {
		supervisor := NewSupervisor(t.TempDir(), time.Second)
		supervisor.processes["active"] = &supervisedProcess{state: processruntime.State{UID: "uid", Phase: phase}}
		require.ErrorContains(t, supervisor.PruneTranscript("active", "uid"), "active process")
		require.Contains(t, supervisor.processes, "active")
	}
}

func TestTranscriptCleanupFencesLateStartAndRetriesFailure(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "late", "transcript.log")
	require.NoError(t, os.MkdirAll(path, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(path, "blocks-remove"), []byte("keep"), 0o600))
	require.Error(t, RemoveTranscript(directory, "late", "uid"), "filesystem failure must be reported")
	require.NoError(t, os.Remove(filepath.Join(path, "blocks-remove")))
	require.NoError(t, RemoveTranscript(directory, "late", "uid"))
	_, err := NewSupervisor(directory, time.Second).Start(t.Context(), processruntime.StartRequest{ID: "late", UID: "uid", Command: []string{"must-never-run"}})
	require.ErrorIs(t, err, processruntime.ErrNotFound)
	for _, id := range []string{"", ".", "..", "../outside"} {
		require.Error(t, RemoveTranscript(directory, id, "uid"))
	}
}
