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

const (
	// WorktreeExecConditionSucceeded reports the terminal result of an exec.
	WorktreeExecConditionSucceeded = "Succeeded"
)

// WorktreeReference identifies a Worktree in the same namespace.
type WorktreeReference struct {
	// name is the name of the Worktree.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// WorktreeExecSpec defines the desired state of WorktreeExec.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type WorktreeExecSpec struct {
	// worktreeRef selects the Worktree whose child volume receives the command.
	// +required
	WorktreeRef WorktreeReference `json:"worktreeRef"`

	// command is the exact argv executed in the Worktree. The first item is the
	// executable and remaining items are its arguments. No shell is added.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MinLength=1
	// +required
	Command []string `json:"command"`
}

// WorktreeExecStatus defines the observed state of WorktreeExec.
type WorktreeExecStatus struct {
	// jobName is the name of the Job executing the command.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// conditions represent the current state of the WorktreeExec resource.
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
// +kubebuilder:printcolumn:name="Worktree",type=string,JSONPath=".spec.worktreeRef.name"
// +kubebuilder:printcolumn:name="Succeeded",type=string,JSONPath=".status.conditions[?(@.type=='Succeeded')].status"
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=".status.jobName"

// WorktreeExec is the Schema for the worktreeexecs API
type WorktreeExec struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WorktreeExec
	// +required
	Spec WorktreeExecSpec `json:"spec"`

	// status defines the observed state of WorktreeExec
	// +optional
	Status WorktreeExecStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorktreeExecList contains a list of WorktreeExec
type WorktreeExecList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WorktreeExec `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &WorktreeExec{}, &WorktreeExecList{})
		return nil
	})
}
