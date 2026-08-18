package repositories

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestRepositoryNamePreservesHostAndNamespacePath(t *testing.T) {
	t.Parallel()

	name, err := RepositoryName("https://gitlab.com/acme/platform/tools.git")

	require.NoError(t, err)
	assert.Equal(t, "gitlab-com-acme-platform-tools", name)
	assert.Empty(t, validation.IsDNS1123Subdomain(name))
}

func TestRepositoryNameAcceptsHTTPSAndSSHURLs(t *testing.T) {
	t.Parallel()

	httpsName, err := RepositoryName("https://gitlab.com/acme/platform/tools.git")
	require.NoError(t, err)
	sshName, err := RepositoryName("git@gitlab.com:acme/platform/tools.git")
	require.NoError(t, err)

	assert.Equal(t, httpsName, sshName)
}

func TestRepositoryNameRejectsInvalidURL(t *testing.T) {
	t.Parallel()

	_, err := RepositoryName("tools.git")

	assert.EqualError(t, err, `invalid Git URL "tools.git": repository URL must include a host and path`)
}

func TestWorktreeNameDerivesStableBranchName(t *testing.T) {
	t.Parallel()
	name := WorktreeName("gitlab-com-acme-tools", "feature/login", "", false, false)
	assert.Equal(t, "gitlab-com-acme-tools-feature-login", name)
	assert.Empty(t, validation.IsDNS1123Subdomain(name))
}

func TestWorktreeNameUsesShortRandomSuffixWithoutStableInput(t *testing.T) {
	t.Parallel()
	first := WorktreeName("repository", "", "", true, false)
	second := WorktreeName("repository", "", "", true, false)
	assert.NotEqual(t, first, second)
	assert.True(t, strings.HasPrefix(first, "repository-detached-"))
	assert.Len(t, strings.TrimPrefix(first, "repository-detached-"), 6)
}
