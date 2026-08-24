/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WorkspaceDesiredState controls whether a runtime Pod should exist.
// +kubebuilder:validation:Enum=Running;Suspended
type WorkspaceDesiredState string

const (
	WorkspaceDesiredStateRunning   WorkspaceDesiredState = "Running"
	WorkspaceDesiredStateSuspended WorkspaceDesiredState = "Suspended"
	WorkspaceConditionReady                              = ConditionReady
	WorkspaceConditionOutdated                           = ConditionOutdated
)

// WorkspaceMount associates a stable path with a Worktree or Repository PVC.
// +kubebuilder:validation:XValidation:rule="has(self.worktreeRef) != has(self.repositoryRef)",message="exactly one of worktreeRef or repositoryRef must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.repositoryRef) || self.readOnly",message="Repository mounts must be read-only"
type WorkspaceMount struct {
	// name identifies this mount and defaults its directory name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`

	// path is relative to /workspace.
	// +kubebuilder:validation:Pattern="^[^/].*$"
	// +required
	Path string `json:"path"`

	// worktreeRef selects a writable or read-only Worktree.
	// +optional
	WorktreeRef *LocalReference `json:"worktreeRef,omitempty"`

	// repositoryRef selects the synchronized Repository parent PVC.
	// +optional
	RepositoryRef *LocalReference `json:"repositoryRef,omitempty"`

	// readOnly mounts the source without write access.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// WorkspaceLifecycleAction runs one exact argv or shell script inside the
// Workspace runtime image.
// +kubebuilder:validation:XValidation:rule="has(self.command) != has(self.script)",message="exactly one of command or script must be set"
type WorkspaceLifecycleAction struct {
	// command is executed as an exact argv without shell interpretation.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +optional
	Command []string `json:"command,omitempty"`

	// script is executed by /bin/sh -ceu.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Script string `json:"script,omitempty"`

	// workingDirectory defaults to /workspace.
	// +optional
	WorkingDirectory string `json:"workingDirectory,omitempty"`
}

// WorkspaceLifecycle configures ordered runtime startup and shutdown actions.
// Initialize actions must succeed before the runtime becomes Ready. BeforeStop
// actions are best-effort Kubernetes pre-stop hooks.
type WorkspaceLifecycle struct {
	// initialize runs in order before the rc-kube supervisor starts. Actions run
	// again whenever Kubernetes creates a replacement runtime Pod.
	// +optional
	Initialize []WorkspaceLifecycleAction `json:"initialize,omitempty"`

	// beforeStop runs in order before a normally terminated runtime container.
	// Kubernetes continues termination if an action fails or the grace period
	// expires.
	// +optional
	BeforeStop []WorkspaceLifecycleAction `json:"beforeStop,omitempty"`
}

// WorkspaceSpec defines one persistent development machine.
type WorkspaceSpec struct {
	// desiredState controls runtime compute while retaining persistent state.
	// +kubebuilder:default=Running
	// +optional
	DesiredState WorkspaceDesiredState `json:"desiredState,omitempty"`

	// environmentRef selects the Environment current revision cloned at creation.
	// +optional
	EnvironmentRef *LocalReference `json:"environmentRef,omitempty"`

	// image is used only for a blank Workspace without environmentRef. When
	// omitted, the controller's base Workspace image is used.
	// +optional
	Image string `json:"image,omitempty"`

	// storage is required for a blank Workspace and ignored when cloning an
	// Environment, whose storage shape is retained.
	// +optional
	Storage *PersistentStorageSpec `json:"storage,omitempty"`

	// mounts exposes Worktrees and explicit read-only Repositories.
	// +listType=map
	// +listMapKey=name
	// +optional
	Mounts []WorkspaceMount `json:"mounts,omitempty"`

	// configMapRefs are projected into the Workspace runtime.
	// +listType=map
	// +listMapKey=name
	// +optional
	ConfigMapRefs []LocalReference `json:"configMapRefs,omitempty"`

	// secretRefs are projected into the Workspace runtime.
	// +listType=map
	// +listMapKey=name
	// +optional
	SecretRefs []LocalReference `json:"secretRefs,omitempty"`

	// agentCredentialRefs are ordered. The first compatible entry is default.
	// +optional
	AgentCredentialRefs []LocalReference `json:"agentCredentialRefs,omitempty"`

	// credentialRefs are generic named credentials available to commands.
	// +optional
	CredentialRefs []LocalReference `json:"credentialRefs,omitempty"`

	// env supplies Workspace-level process defaults.
	// +listType=map
	// +listMapKey=name
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// defaultWorkingDirectory is used when a process does not set --cwd.
	// +optional
	DefaultWorkingDirectory string `json:"defaultWorkingDirectory,omitempty"`

	// lifecycle configures commands and scripts around each runtime Pod.
	// +optional
	Lifecycle *WorkspaceLifecycle `json:"lifecycle,omitempty"`

	// serviceAccountName overrides the namespace rc-workspace ServiceAccount.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// automountServiceAccountToken disables Kubernetes access when false.
	// +optional
	AutomountServiceAccountToken *bool `json:"automountServiceAccountToken,omitempty"`

	// resources are shared by all processes in the runtime Pod.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// nodeSelector constrains runtime placement.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations configure runtime placement.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// affinity configures runtime placement.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// runtimeClassName selects a RuntimeClass for the runtime Pod.
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`

	// idleTimeout suspends an idle named Workspace. Zero or omitted disables it.
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// generated marks a Workspace created by rcctl for one invocation.
	// +optional
	Generated bool `json:"generated,omitempty"`
}

// WorkspaceStatus defines the observed state of Workspace.
type WorkspaceStatus struct {
	// observedGeneration is the latest generation reflected by status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// sourceEnvironmentRevision records the cloned Environment revision.
	// +optional
	SourceEnvironmentRevision int64 `json:"sourceEnvironmentRevision,omitempty"`

	// runtimeImage is the exact image string captured at Workspace creation.
	// +optional
	RuntimeImage string `json:"runtimeImage,omitempty"`

	// homeVolumeClaimName contains mutable Workspace home state and transcripts.
	// +optional
	HomeVolumeClaimName string `json:"homeVolumeClaimName,omitempty"`

	// runtimePodName is the active runtime Pod when desiredState is Running.
	// +optional
	RuntimePodName string `json:"runtimePodName,omitempty"`

	// lastAutoSuspendTime records the newest process completion consumed by
	// generated-Workspace auto-suspension.
	// +optional
	LastAutoSuspendTime *metav1.Time `json:"lastAutoSuspendTime,omitempty"`

	// conditions represent the current state of the Workspace resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=".spec.desiredState"
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=".status.runtimeImage"
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=".status.runtimePodName"

// Workspace is the Schema for the workspaces API
type Workspace struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Workspace
	// +required
	Spec WorkspaceSpec `json:"spec"`

	// status defines the observed state of Workspace
	// +optional
	Status WorkspaceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkspaceList contains a list of Workspace
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Workspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Workspace{}, &WorkspaceList{})
		return nil
	})
}
