package repositories

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/validation"
)

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
