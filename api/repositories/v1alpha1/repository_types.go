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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

const (
	// RepositoryConditionStorageReady reports whether the parent volume can
	// accept an exec request.
	RepositoryConditionStorageReady = "StorageReady"
)

// RepositoryCredentialReference selects a Credential in the same namespace as
// the Repository.
type RepositoryCredentialReference struct {
	// name is the name of the Credential.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// RepositoryRemoteSpec identifies the Git remote synchronized into a
// Repository.
// +kubebuilder:validation:XValidation:rule="!self.url.matches('(?i)^http://') || (has(self.allowInsecureHTTP) && self.allowInsecureHTTP)",message="allowInsecureHTTP must be true for an http:// remote"
// +kubebuilder:validation:XValidation:rule="!has(self.allowInsecureHTTP) || !self.allowInsecureHTTP || self.url.matches('(?i)^http://')",message="allowInsecureHTTP may only be set for an http:// remote"
type RepositoryRemoteSpec struct {
	// url is the Git remote URL.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +required
	URL string `json:"url"`

	// credentialRef selects a Credential in the same namespace.
	// +optional
	CredentialRef *RepositoryCredentialReference `json:"credentialRef,omitempty"`

	// allowInsecureHTTP explicitly permits an unencrypted HTTP remote.
	// +optional
	AllowInsecureHTTP bool `json:"allowInsecureHTTP,omitempty"`

	// TODO(repository-source-validation): Complete Git URL validation and
	// Credential compatibility checks are deferred until the sync controller
	// consumes this source contract.
}

// RepositorySubmodulesSpec configures Git submodule initialization during
// Repository synchronization. The presence of this object enables submodules.
type RepositorySubmodulesSpec struct {
	// recursive initializes nested submodules in addition to direct submodules.
	// +optional
	Recursive bool `json:"recursive,omitempty"`
}

// RepositoryStorageSpec describes the persistent parent volume.
// +kubebuilder:validation:XValidation:rule="quantity(self.size).isGreaterThan(quantity('0'))",message="size must be greater than zero"
type RepositoryStorageSpec struct {
	// storageClassName is the StorageClass used to provision the parent volume.
	// +kubebuilder:validation:MinLength=1
	// +required
	StorageClassName string `json:"storageClassName"`

	// size is the requested capacity of the parent volume.
	// +required
	Size resource.Quantity `json:"size"`
}

// RepositorySpec defines the desired state of Repository.
type RepositorySpec struct {
	// remote identifies the Git remote synchronized into the Repository.
	// +required
	Remote RepositoryRemoteSpec `json:"remote"`

	// ref selects a full Git ref, or a full SHA-1 or SHA-256 commit. When
	// omitted, the remote's default branch is used.
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern="^(refs/.+|[0-9A-Fa-f]{40}|[0-9A-Fa-f]{64})$"
	// +optional
	Ref string `json:"ref,omitempty"`

	// submodules configures optional Git submodule initialization. When omitted,
	// the Repository synchronizes only the parent checkout.
	// +optional
	Submodules *RepositorySubmodulesSpec `json:"submodules,omitempty"`

	// storage configures the persistent parent volume.
	// +required
	Storage RepositoryStorageSpec `json:"storage"`
}

// RepositoryStatus defines the observed state of Repository.
type RepositoryStatus struct {
	// observedGeneration is the latest Repository generation reflected by this
	// status. This is Kubernetes object bookkeeping, not a Repository data
	// generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// volumeClaimName is the name of the parent PersistentVolumeClaim.
	// +optional
	VolumeClaimName string `json:"volumeClaimName,omitempty"`

	// lastUpdatedAt is the completion time of the last successful Git
	// synchronization Job that updated the parent volume.
	// +optional
	LastUpdatedAt *metav1.Time `json:"lastUpdatedAt,omitempty"`

	// conditions represent the current state of the Repository resource.
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
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='StorageReady')].status"
// +kubebuilder:printcolumn:name="Remote",type=string,JSONPath=".spec.remote.url"
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=".spec.ref"
// +kubebuilder:printcolumn:name="Updated",type=date,JSONPath=".status.lastUpdatedAt"
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=".status.volumeClaimName"

// Repository is the Schema for the repositories API
type Repository struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Repository
	// +required
	Spec RepositorySpec `json:"spec"`

	// status defines the observed state of Repository
	// +optional
	Status RepositoryStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RepositoryList contains a list of Repository
type RepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Repository `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Repository{}, &RepositoryList{})
		return nil
	})
}
