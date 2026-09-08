package rckube

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsGitSSHUsesOnlySelectedFragments(t *testing.T) {
	// Process-wide Git overrides must not influence this configuration test.
	t.Setenv("GIT_SSH_COMMAND", "")
	require.NoError(t, os.Unsetenv("GIT_SSH_COMMAND"))
	t.Setenv("GIT_SSH", "")
	require.NoError(t, os.Unsetenv("GIT_SSH"))
	root := t.TempDir()
	selected := filepath.Join(root, "selected.conf")
	unselected := filepath.Join(root, "unselected.conf")
	require.NoError(t, os.WriteFile(selected, []byte("Host github.com\n  IdentityFile C:/run/rc/credentials/github/id\n"), 0o600))
	require.NoError(t, os.WriteFile(unselected, []byte("Host private.example\n  User unselected\n"), 0o600))
	environment := map[string]string{}
	require.NoError(t, configureWindowsGitSSH(environment, root, []string{selected}))
	config, err := os.ReadFile(filepath.Join(root, "git-ssh.conf"))
	require.NoError(t, err)
	assert.Contains(t, string(config), "IdentityFile C:/run/rc/credentials/github/id")
	assert.NotContains(t, string(config), "Include")
	assert.NotContains(t, string(config), "unselected")
	assert.Contains(t, environment["GIT_SSH_COMMAND"], "ssh -F ")
}

func TestWindowsGitSSHPreservesExplicitTransport(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct{ name, variable string }{
		{name: "Command", variable: "GIT_SSH_COMMAND"},
		{name: "MixedCaseCommand", variable: "Git_Ssh_Command"},
		{name: "Executable", variable: "GIT_SSH"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			environment := map[string]string{testCase.variable: "custom-ssh"}
			require.NoError(t, configureWindowsGitSSH(environment, root, []string{"must-not-read.conf"}))
			assert.Equal(t, map[string]string{testCase.variable: "custom-ssh"}, environment)
			assert.NoFileExists(t, filepath.Join(root, "git-ssh.conf"))
		})
	}
}

func TestWindowsGitSSHLeavesUncredentialedProcessesUnchanged(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	environment := map[string]string{}
	require.NoError(t, configureWindowsGitSSH(environment, root, nil))
	assert.Empty(t, environment)
	assert.NoFileExists(t, filepath.Join(root, "git-ssh.conf"))
}
