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
)

const (
	// ConditionReady reports whether a Workspace resource can serve new work.
	ConditionReady = "Ready"
	// ConditionOutdated reports whether a Workspace was cloned from an older
	// Environment revision or image value.
	ConditionOutdated = "Outdated"
	// ConditionStorageReady reports whether the target's persistent volume claim
	// exists, is owned by the target, and is bound.
	ConditionStorageReady = "StorageReady"
	// ConditionExecutionHistoryCompliant reports whether the target's
	// WorkspaceExec history has no removals waiting for cleanup.
	ConditionExecutionHistoryCompliant = "ExecutionHistoryCompliant"
)

// Condition reasons published by the workspaces controllers.
const (
	// ReasonVolumeClaimBound means StorageReady=True.
	ReasonVolumeClaimBound = "VolumeClaimBound"
	// ReasonVolumeClaimLost means a previously recorded PVC no longer exists. The
	// controller does not recreate it, because that would silently discard state.
	ReasonVolumeClaimLost = "VolumeClaimLost"
	// ReasonVolumeClaimConflict means the PVC is not controlled by this target
	// incarnation or is terminating.
	ReasonVolumeClaimConflict = "VolumeClaimConflict"
	// ReasonWithinPolicy means ExecutionHistoryCompliant=True under a policy.
	ReasonWithinPolicy = "WithinPolicy"
	// ReasonPolicyUnset means ExecutionHistoryCompliant=True without a policy:
	// history is kept until deleted explicitly.
	ReasonPolicyUnset = "PolicyUnset"
	// ReasonCleanupBacklog means ExecutionHistoryCompliant=False: some records
	// are expired or deleting and still wait for cleanup.
	ReasonCleanupBacklog = "CleanupBacklog"
)

// ExecutionHistoryStatus is the controller's view of a target's WorkspaceExec
// history under its executionRetention policy.
type ExecutionHistoryStatus struct {
	// retained counts this target's WorkspaceExec records that the policy keeps,
	// including active and pinned records.
	// +optional
	Retained int32 `json:"retained"`

	// pendingCleanup counts records that are expired under the policy or
	// already deleting, and still exist. Removals are bounded per pass, so a
	// large backlog drains over several passes.
	// +optional
	PendingCleanup int32 `json:"pendingCleanup"`

	// effectiveTTL is ttlAfterFinished after defaulting. It is empty when no
	// policy is set.
	// +optional
	EffectiveTTL *metav1.Duration `json:"effectiveTTL,omitempty"`

	// effectiveMaxEntries is maxEntries after defaulting. It is zero when no
	// policy is set.
	// +optional
	EffectiveMaxEntries int32 `json:"effectiveMaxEntries,omitempty"`

	// observedGeneration is the target generation this summary reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// LocalReference selects an rc resource in the same namespace.
type LocalReference struct {
	// name is the metadata.name of the referenced resource.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// PersistentStorageSpec configures an rc-managed PersistentVolumeClaim.
type PersistentStorageSpec struct {
	// storageClassName is the StorageClass used to provision the volume. When
	// omitted, Kubernetes selects the cluster default StorageClass.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// size is the requested storage capacity.
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="size must be greater than zero"
	// +required
	Size resource.Quantity `json:"size"`

	// accessModes controls how the volume may be mounted. rc defaults to
	// ReadWriteOnce when this field is omitted.
	// +kubebuilder:validation:Items:Enum=ReadWriteOnce;ReadOnlyMany;ReadWriteMany;ReadWriteOncePod
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	// volumeMode controls whether the volume is exposed as a filesystem or a
	// block device. Workspace runtime volumes require Filesystem.
	// +kubebuilder:validation:Enum=Filesystem
	// +optional
	VolumeMode *corev1.PersistentVolumeMode `json:"volumeMode,omitempty"`
}

func (storage PersistentStorageSpec) AccessModesOrDefault() []corev1.PersistentVolumeAccessMode {
	if len(storage.AccessModes) == 0 {
		return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}

	return append([]corev1.PersistentVolumeAccessMode(nil), storage.AccessModes...)
}

func (storage PersistentStorageSpec) VolumeModeOrDefault() corev1.PersistentVolumeMode {
	if storage.VolumeMode == nil {
		return corev1.PersistentVolumeFilesystem
	}

	return *storage.VolumeMode
}
