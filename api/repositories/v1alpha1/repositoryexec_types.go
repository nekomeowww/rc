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

const (
	// RepositoryExecConditionSucceeded reports the terminal result of an exec.
	RepositoryExecConditionSucceeded = "Succeeded"
)

// RepositoryReference identifies a Repository in the same namespace.
type RepositoryReference struct {
	// name is the name of the Repository.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// RepositoryExecSpec defines the desired state of RepositoryExec.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type RepositoryExecSpec struct {
	// repositoryRef selects the Repository whose parent volume receives the
	// command.
	// +required
	RepositoryRef RepositoryReference `json:"repositoryRef"`

	// command is the exact argv executed in the Repository. The first item is
	// the executable and remaining items are its arguments. No shell is added.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MinLength=1
	// +required
	Command []string `json:"command"`
}

// RepositoryExecStatus defines the observed state of RepositoryExec.
type RepositoryExecStatus struct {
	// jobName is the name of the Job executing the command.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// conditions represent the current state of the RepositoryExec resource.
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
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=".spec.repositoryRef.name"
// +kubebuilder:printcolumn:name="Succeeded",type=string,JSONPath=".status.conditions[?(@.type=='Succeeded')].status"
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=".status.jobName"

// RepositoryExec is the Schema for the repositoryexecs API
type RepositoryExec struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RepositoryExec
	// +required
	Spec RepositoryExecSpec `json:"spec"`

	// status defines the observed state of RepositoryExec
	// +optional
	Status RepositoryExecStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RepositoryExecList contains a list of RepositoryExec
type RepositoryExecList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RepositoryExec `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &RepositoryExec{}, &RepositoryExecList{})
		return nil
	})
}
