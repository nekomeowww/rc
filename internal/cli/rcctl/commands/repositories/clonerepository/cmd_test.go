package clonerepository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nekomeowww/rc/internal/kubeconfig"
)

const (
	testCloneURL         = "https://gitlab.com/acme/platform/tools.git"
	testStorageClassFlag = "--storage-class"
	testStorageClass     = "truenas-nfs"
)

func TestCloneAcceptsRepositoryOptions(t *testing.T) {
	t.Parallel()

	command := NewCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{
		testCloneURL,
		testStorageClassFlag, testStorageClass,
		"--size", "10Gi",
		"--ref", "refs/heads/main",
		"--credential-ref", "gitlab-token",
		"--name", "tools-main",
	}))
	require.NoError(t, command.Args(command, command.Flags().Args()))

	assert.Equal(t, []string{testCloneURL}, command.Flags().Args())
}

func TestCloneDefaultsRepositorySize(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)

	command := NewCommand(kubeconfig.NewFlags())
	requirements.NoError(command.ParseFlags([]string{
		testCloneURL,
		testStorageClassFlag, testStorageClass,
	}))
	requirements.NoError(command.PreRunE(command, command.Flags().Args()), "validate the default Repository size")

	sizeFlag := command.Flags().Lookup("size")
	requirements.NotNil(sizeFlag, "register the Repository size flag")
	assertions.Equal(defaultRepositorySize, sizeFlag.Value.String(), "default Repository parent PVCs to 20Gi")
}

func TestCloneRequiresURL(t *testing.T) {
	t.Parallel()

	command := NewCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{
		testStorageClassFlag, testStorageClass,
		"--size", "10Gi",
	}))

	assert.EqualError(t, command.Args(command, command.Flags().Args()), "accepts 1 arg(s), received 0")
}

func TestParseSizeRejectsNonPositiveValues(t *testing.T) {
	t.Parallel()

	_, err := parseSize("0Gi")

	assert.EqualError(t, err, "--size must be greater than zero")
}
