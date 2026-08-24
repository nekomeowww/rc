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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// WorktreeConditionVolumeReady reports whether the cloned child PVC can be
	// mounted. Generated Workspace Worktrees may become volume-ready before the
	// runtime initializes their Git branch.
	WorktreeConditionVolumeReady = "VolumeReady"

	// WorktreeConditionReady reports whether the child volume and its native
	// Git worktree are ready for a workload to mount.
	WorktreeConditionReady = "Ready"
)

// WorktreeStorageSpec optionally overrides the storage inherited from the
// referenced Repository.
type WorktreeStorageSpec struct {
	// storageClassName is the StorageClass used to provision the child volume.
	// +kubebuilder:validation:MinLength=1
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// size is the requested capacity of the child volume.
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="size must be greater than zero"
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// accessModes controls how workloads may mount the child volume. When
	// omitted, the controller requests ReadWriteMany so a workload and a
	// parallel inspection Pod can mount the worktree together.
	// +kubebuilder:validation:Items:Enum=ReadWriteOnce;ReadOnlyMany;ReadWriteMany;ReadWriteOncePod
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
}

// WorktreeSpec defines one independent child volume and the native Git
// worktree that is created inside it.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; recreate the Worktree to change its source or Git add options"
// +kubebuilder:validation:XValidation:rule="!(has(self.branch) && has(self.resetBranch))",message="branch and resetBranch are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!has(self.lockReason) || (has(self.lock) && self.lock)",message="lockReason requires lock=true"
type WorktreeSpec struct {
	// repositoryRef selects the Repository parent PVC to clone.
	// +required
	RepositoryRef RepositoryReference `json:"repositoryRef"`

	// branch creates a new local branch in the child Git repository.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Branch string `json:"branch,omitempty"`

	// resetBranch creates or resets a local branch in the child Git repository.
	// This is the destructive -B equivalent.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	ResetBranch string `json:"resetBranch,omitempty"`

	// ref is the commit-ish checked out by the new worktree.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Ref string `json:"ref,omitempty"`

	// detach creates a detached HEAD worktree.
	// +optional
	Detach bool `json:"detach,omitempty"`

	// orphan creates an unborn branch.
	// +optional
	Orphan bool `json:"orphan,omitempty"`

	// noCheckout creates the worktree metadata without populating files.
	// +optional
	NoCheckout bool `json:"noCheckout,omitempty"`

	// lock keeps the native Git worktree locked after creation.
	// +optional
	Lock bool `json:"lock,omitempty"`

	// lockReason records why the native Git worktree is locked.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	LockReason string `json:"lockReason,omitempty"`

	// storage optionally overrides the Repository storage settings.
	// +optional
	Storage *WorktreeStorageSpec `json:"storage,omitempty"`
}

// WorktreeStatus defines the observed state of Worktree.
type WorktreeStatus struct {
	// observedGeneration is the latest Worktree API generation reflected by
	// this status. It is Kubernetes object bookkeeping, not a Repository data
	// generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// sourceVolumeClaimName is the parent PVC cloned when this Worktree was
	// created.
	// +optional
	SourceVolumeClaimName string `json:"sourceVolumeClaimName,omitempty"`

	// volumeClaimName is the child PVC containing the independent Git copy.
	// +optional
	VolumeClaimName string `json:"volumeClaimName,omitempty"`

	// worktreePath is the path inside the child PVC containing the native Git
	// worktree.
	// +optional
	WorktreePath string `json:"worktreePath,omitempty"`

	// jobName is the bootstrap Job that ran git worktree add. It is empty when a
	// generated Workspace runtime initializes the cloned Repository root.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// conditions represent the current state of the Worktree resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:selectablefield:JSONPath=".spec.repositoryRef.name"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=".spec.repositoryRef.name"
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=".status.volumeClaimName"
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=".status.worktreePath"
type Worktree struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Worktree
	// +required
	Spec WorktreeSpec `json:"spec"`

	// status defines the observed state of Worktree
	// +optional
	Status WorktreeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorktreeList contains a list of Worktree.
type WorktreeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Worktree `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Worktree{}, &WorktreeList{})
		return nil
	})
}
