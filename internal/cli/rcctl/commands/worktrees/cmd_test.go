package worktrees

import (
	"testing"

	"github.com/nekomeowww/rc/internal/kubeconfig"
	"github.com/stretchr/testify/require"
)

func TestAddHasNoPositionalArgumentsAndAcceptsGitStyleFlags(t *testing.T) {
	t.Parallel()
	command := newAddCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{
		"--repo", "gitlab.com/acme/tools",
		"-b", "feature/login",
		"--name", "tools-feature-login",
		"--detach=false",
	}))
	require.NoError(t, command.Args(command, command.Flags().Args()))
}
