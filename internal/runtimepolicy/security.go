package runtimepolicy

import corev1 "k8s.io/api/core/v1"

const AgentUserID int64 = 1000

// PodSecurityContext keeps Linux UID/FSGroup/seccomp settings off Windows Pods.
// Windows development images use the container-local administrator for tool
// installation and credential symlinks; they never request HostProcess access.
func PodSecurityContext(os corev1.OSName) *corev1.PodSecurityContext {
	if os == "darwin" {
		return nil
	}
	if os == corev1.Windows {
		user := "ContainerAdministrator"
		return &corev1.PodSecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{RunAsUserName: &user}}
	}
	return AgentPodSecurityContext()
}

func ContainerSecurityContext(os corev1.OSName) *corev1.SecurityContext {
	if os == "darwin" {
		return nil
	}
	if os == corev1.Windows {
		return nil
	}
	disallow := false
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &disallow, ReadOnlyRootFilesystem: &disallow,
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

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
