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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CredentialType describes how a consumer interprets credential data.
// +kubebuilder:validation:Enum=SSHPrivateKey;APIKey;OAuth
type CredentialType string

const (
	// CredentialTypeSSHPrivateKey identifies an SSH private key.
	CredentialTypeSSHPrivateKey CredentialType = "SSHPrivateKey"
	// CredentialTypeAPIKey identifies an API key.
	CredentialTypeAPIKey CredentialType = "APIKey"
	// CredentialTypeOAuth identifies OAuth credential data.
	CredentialTypeOAuth CredentialType = "OAuth"
)

// SecretKeyReference selects one data entry from a Secret in the same
// namespace as the referring resource.
type SecretKeyReference struct {
	// name is the name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// key is the key of the Secret data entry containing the credential.
	// +kubebuilder:validation:MinLength=1
	// +required
	Key string `json:"key"`
}

// CredentialSpec defines the desired state of Credential.
type CredentialSpec struct {
	// type describes how consumers interpret the referenced secret data.
	// +required
	Type CredentialType `json:"type"`

	// secretKeyRef selects the credential data from a Secret in the same
	// namespace as this Credential.
	// +required
	SecretKeyRef SecretKeyReference `json:"secretKeyRef"`
}

// CredentialStatus defines the observed state of Credential.
type CredentialStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Credential resource.
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

// Credential is the Schema for the credentials API
type Credential struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Credential
	// +required
	Spec CredentialSpec `json:"spec"`

	// status defines the observed state of Credential
	// +optional
	Status CredentialStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CredentialList contains a list of Credential
type CredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Credential `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Credential{}, &CredentialList{})
		return nil
	})
}
