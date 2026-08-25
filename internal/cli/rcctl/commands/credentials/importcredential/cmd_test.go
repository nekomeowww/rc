package importcredential

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nekomeowww/rc/internal/kubeconfig"
)

const (
	testGitHubCredentialType = credentialTypeGitHub
	testGitHubHostname       = defaultGitHubHost
)

func TestValidateAcceptsGitHubImportWithoutFile(t *testing.T) {
	t.Parallel()

	options := options{
		credentialType: testGitHubCredentialType,
		hostname:       testGitHubHostname,
		name:           "github-com",
	}

	require.NoError(t, options.validate())
}

func TestValidateRejectsFileForGitHubImport(t *testing.T) {
	t.Parallel()

	options := options{
		credentialType: testGitHubCredentialType,
		hostname:       testGitHubHostname,
		file:           "token.txt",
	}

	assert.EqualError(t, options.validate(), "flag --file is only supported with --type agent or process")
}

func TestImportCommandAcceptsGitHubFlags(t *testing.T) {
	t.Parallel()

	command := NewCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{
		"--type", testGitHubCredentialType,
		"--hostname", "github.example.com",
		"--name", "github-enterprise",
	}))
	require.NoError(t, command.Args(command, command.Flags().Args()))
}

func TestValidateAcceptsProcessCredentialImport(t *testing.T) {
	t.Parallel()

	require.NoError(t, (options{
		credentialType: credentialTypeProcess,
		name:           "tool-auth",
		file:           "/tmp/credentials.json",
		mountPath:      "/home/agent/.tool/credentials.json",
		environment:    []string{"TOOL_HOME=/home/agent/.tool"},
	}).validate())
}
