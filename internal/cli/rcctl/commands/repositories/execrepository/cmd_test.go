package execrepository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nekomeowww/rc/internal/kubeconfig"
)

const (
	testGitCommand     = "git"
	testRepositoryName = "auv"
	testStatusArgument = "status"
)

func TestExactCommandArgsRequireDelimiterAndPreserveFlags(t *testing.T) {
	t.Parallel()

	command := NewCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{testRepositoryName, "--", testGitCommand, testStatusArgument, "--short"}))
	args := command.Flags().Args()
	require.NoError(t, exactCommandArgs(command, args))
	assert.Equal(t, []string{testRepositoryName, testGitCommand, testStatusArgument, "--short"}, args)
}

func TestExactCommandArgsRejectMissingDelimiter(t *testing.T) {
	t.Parallel()

	command := NewCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{testRepositoryName, testGitCommand, testStatusArgument}))
	assert.EqualError(t, exactCommandArgs(command, command.Flags().Args()), "expected exactly one Repository name before --")
}
