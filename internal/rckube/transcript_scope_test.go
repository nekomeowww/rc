package rckube

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTranscriptScopePreservesRestartsAndPrunesInheritedCopies(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	processDirectory := filepath.Join(directory, "old-process")
	require.NoError(t, os.MkdirAll(processDirectory, 0o700))
	transcript := filepath.Join(processDirectory, "transcript.log")
	owner := filepath.Join(processDirectory, "owner.uid")
	require.NoError(t, os.WriteFile(transcript, []byte("source debug log"), 0o600))
	require.NoError(t, os.WriteFile(owner, []byte("uid"), 0o600))
	require.NoError(t, PrepareTranscriptScope(directory, "environment/draft-1"))
	require.FileExists(t, transcript, "legacy volumes are adopted without losing history")
	require.NoError(t, PrepareTranscriptScope(directory, "environment/draft-1"))
	require.FileExists(t, transcript, "restarting the same home preserves history")
	// ROOT CAUSE: PVC cloning also copied .rc/processes; a CR-based collector
	// cannot enumerate those independent descendants once they become Workspaces.
	require.NoError(t, PrepareTranscriptScope(directory, "workspace/independent"))
	require.NoFileExists(t, transcript, "a cloned target must not inherit the source's transcripts")
	require.FileExists(t, owner, "never erase an at-most-once tombstone")
	require.NoError(t, os.WriteFile(transcript, []byte("new target history"), 0o600))
	require.NoError(t, PrepareTranscriptScope(directory, "workspace/independent"))
	require.FileExists(t, transcript)
}
