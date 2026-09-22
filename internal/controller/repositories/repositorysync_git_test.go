package repositories

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSyncAdvancesParentAndPreservesDirtyChild(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Repository Jobs run the POSIX script on Linux")
	}
	directory := t.TempDir()
	remote := filepath.Join(directory, "remote")
	parent := filepath.Join(directory, "parent")
	child := filepath.Join(directory, "child")
	require.NoError(t, os.MkdirAll(remote, 0755))
	require.NoError(t, os.MkdirAll(parent, 0755))
	git := func(path string, args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", args...)
		command.Dir = path
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(directory, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		return strings.TrimSpace(string(output))
	}
	git(remote, "init", "-b", "main")
	git(remote, "config", "user.email", "sync@example.test")
	git(remote, "config", "user.name", "Sync test")
	file := filepath.Join(remote, "tracked.txt")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0644))
	git(remote, "add", ".")
	git(remote, "commit", "-m", "first")
	first := git(remote, "rev-parse", "HEAD")
	synchronize := func() {
		t.Helper()
		// Substitute only the container mount path; execute the production fetch,
		// ref resolution, reset, clean, and submodule policy against real Git.
		script := strings.ReplaceAll(repositoryBootstrapScript, "/repository", parent)
		command := exec.CommandContext(t.Context(), "sh", "-ceu", script, "repository-sync", remote, "refs/heads/main", "none", "none", "0")
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(directory, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
	}
	synchronize()
	git(directory, "clone", parent, child)
	require.NoError(t, os.WriteFile(filepath.Join(child, "tracked.txt"), []byte("local edit"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(child, "untracked.txt"), []byte("keep"), 0644))
	require.NoError(t, os.WriteFile(file, []byte("second"), 0644))
	git(remote, "commit", "-am", "second")
	second := git(remote, "rev-parse", "HEAD")
	synchronize()
	require.Equal(t, second, git(parent, "rev-parse", "HEAD"))
	require.Equal(t, first, git(child, "rev-parse", "HEAD"))
	contents, err := os.ReadFile(filepath.Join(child, "tracked.txt"))
	require.NoError(t, err)
	require.Equal(t, "local edit", string(contents))
	contents, err = os.ReadFile(filepath.Join(child, "untracked.txt"))
	require.NoError(t, err)
	require.Equal(t, "keep", string(contents))
	fresh := filepath.Join(directory, "fresh")
	git(directory, "clone", parent, fresh)
	require.Equal(t, second, git(fresh, "rev-parse", "HEAD"))
}
