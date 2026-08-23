package runtimepolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestAgentPodSecurityContext(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)

	securityContext := AgentPodSecurityContext()
	requirements.NotNil(securityContext.RunAsNonRoot)
	requirements.NotNil(securityContext.RunAsUser)
	requirements.NotNil(securityContext.RunAsGroup)
	requirements.NotNil(securityContext.FSGroup)
	requirements.NotNil(securityContext.FSGroupChangePolicy)
	requirements.NotNil(securityContext.SeccompProfile)
	assertions.True(*securityContext.RunAsNonRoot)
	assertions.Equal(AgentUserID, *securityContext.RunAsUser)
	assertions.Equal(AgentUserID, *securityContext.RunAsGroup)
	assertions.Equal(AgentUserID, *securityContext.FSGroup)
	assertions.Equal(corev1.FSGroupChangeOnRootMismatch, *securityContext.FSGroupChangePolicy)
	assertions.Equal(corev1.SeccompProfileTypeRuntimeDefault, securityContext.SeccompProfile.Type)

	assertions.NotSame(securityContext, AgentPodSecurityContext(), "return an independently mutable context for every Pod")
}
