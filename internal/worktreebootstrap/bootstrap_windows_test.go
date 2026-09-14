//go:build windows

package worktreebootstrap

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nekomeowww/rc/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func TestWindowsCheckoutPreservesDirtyFilesAndReportsFailure(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		command := exec.Command("git", args...)
		command.Dir = root
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return string(output)
	}
	git("init")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial")
	action := WindowsAction("feature/it's-ready", root)
	require.NoError(t, lifecycle.Run(context.Background(), []lifecycle.Action{action}, nil, io.Discard, io.Discard))
	require.Equal(t, "feature/it's-ready\n", git("branch", "--show-current"))
	marker := filepath.Join(root, "dirty.txt")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0600))
	require.NoError(t, lifecycle.Run(context.Background(), []lifecycle.Action{action}, nil, io.Discard, io.Discard))
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "keep", string(data))
	require.Error(t, lifecycle.Run(context.Background(), []lifecycle.Action{WindowsAction("invalid..branch", root)}, nil, io.Discard, io.Discard))
}
