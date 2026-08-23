package runtimepolicy

import corev1 "k8s.io/api/core/v1"

const AgentUserID int64 = 1000

// AgentPodSecurityContext keeps persistent volume ownership compatible across
// Repository, Worktree, Environment, and Workspace workloads. A matching root
// directory lets kubelet skip an otherwise recursive ownership walk.
func AgentPodSecurityContext() *corev1.PodSecurityContext {
	runAsNonRoot := true
	runAsUser := AgentUserID
	runAsGroup := AgentUserID
	fsGroupChangePolicy := corev1.FSGroupChangeOnRootMismatch

	return &corev1.PodSecurityContext{
		RunAsNonRoot:        &runAsNonRoot,
		RunAsUser:           &runAsUser,
		RunAsGroup:          &runAsGroup,
		FSGroup:             &runAsGroup,
		FSGroupChangePolicy: &fsGroupChangePolicy,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}
