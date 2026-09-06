package lifecycle

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsLifecyclePreservesUnicodeAndOrder(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	actions := []Action{
		{Command: []string{"powershell.exe", "-NoProfile", "-Command", "[IO.File]::WriteAllText('result.txt', 'first')"}, WorkingDirectory: directory},
		{Script: "[IO.File]::AppendAllText('result.txt', '-雪 $HOME')", WorkingDirectory: directory},
	}
	encoded, err := Encode(actions)
	require.NoError(t, err)
	decoded, err := Decode(encoded)
	require.NoError(t, err)
	require.NoError(t, Run(context.Background(), decoded, nil, io.Discard, io.Discard))
	data, err := os.ReadFile(filepath.Join(directory, "result.txt"))
	require.NoError(t, err)
	assert.Equal(t, "first-雪 $HOME", string(data))
}

func TestWindowsLifecycleFailureStopsInitialization(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	actions := []Action{
		{Script: "throw 'initialization failed'", WorkingDirectory: directory},
		{Script: "[IO.File]::WriteAllText('unexpected', 'bad')", WorkingDirectory: directory},
	}
	require.Error(t, Run(context.Background(), actions, nil, io.Discard, io.Discard))
	_, err := os.Stat(filepath.Join(directory, "unexpected"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
