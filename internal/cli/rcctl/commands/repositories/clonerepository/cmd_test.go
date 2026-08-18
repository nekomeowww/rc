package clonerepository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nekomeowww/rc/internal/kubeconfig"
)

func TestCloneAcceptsRepositoryOptions(t *testing.T) {
	t.Parallel()

	command := NewCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{
		"https://gitlab.com/acme/platform/tools.git",
		"--storage-class", "truenas-nfs",
		"--size", "10Gi",
		"--ref", "refs/heads/main",
		"--credential-ref", "gitlab-token",
		"--name", "tools-main",
	}))
	require.NoError(t, command.Args(command, command.Flags().Args()))

	assert.Equal(t, []string{"https://gitlab.com/acme/platform/tools.git"}, command.Flags().Args())
}

func TestCloneRequiresURL(t *testing.T) {
	t.Parallel()

	command := NewCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{
		"--storage-class", "truenas-nfs",
		"--size", "10Gi",
	}))

	assert.EqualError(t, command.Args(command, command.Flags().Args()), "accepts 1 arg(s), received 0")
}

func TestParseSizeRejectsNonPositiveValues(t *testing.T) {
	t.Parallel()

	_, err := parseSize("0Gi")

	assert.EqualError(t, err, "--size must be greater than zero")
}
