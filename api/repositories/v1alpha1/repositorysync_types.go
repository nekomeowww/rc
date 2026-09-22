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

// RepositorySyncConditionSucceeded reports this request's immutable terminal result.
const RepositorySyncConditionSucceeded = "Succeeded"

// RepositorySyncSpec selects the parent mirror to synchronize.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type RepositorySyncSpec struct {
	// repositoryRef selects a Repository in this namespace.
	// +required
	RepositoryRef RepositoryReference `json:"repositoryRef"`

	// ttlSecondsAfterFinished controls how long the completed request is retained.
	// Defaults to 259200 seconds (3 days). Zero makes it eligible for immediate
	// deletion after its consumers stop and the parent reservation is released.
	// +kubebuilder:default=259200
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// RepositorySyncStatus retains the result after Job cleanup.
type RepositorySyncStatus struct {
	// jobName identifies the one Job admitted for this request.
	// +optional
	JobName string `json:"jobName,omitempty"`
	// repositoryUID identifies the Repository incarnation used by this request.
	// +optional
	RepositoryUID string `json:"repositoryUID,omitempty"`

	// repositoryGeneration is the configuration captured when the Job starts.
	// +optional
	RepositoryGeneration int64 `json:"repositoryGeneration,omitempty"`
	// commit is the resolved Git commit after a successful sync.
	// +optional
	Commit string `json:"commit,omitempty"`
	// completedAt is the Job completion time, or the time a failure without a
	// completed Job was recorded.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// conditions report waiting, running, or the terminal result.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=".spec.repositoryRef.name"
// +kubebuilder:printcolumn:name="Succeeded",type=string,JSONPath=".status.conditions[?(@.type=='Succeeded')].status"
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=".status.commit"
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=".status.completedAt"

// RepositorySync is the Schema for the repositorysyncs API
type RepositorySync struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RepositorySync
	// +required
	Spec RepositorySyncSpec `json:"spec"`

	// status defines the observed state of RepositorySync
	// +optional
	Status RepositorySyncStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RepositorySyncList contains a list of RepositorySync
type RepositorySyncList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RepositorySync `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &RepositorySync{}, &RepositorySyncList{})
		return nil
	})
}
