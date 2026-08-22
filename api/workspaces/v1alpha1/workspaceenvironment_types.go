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
	// WorkspaceEnvironmentConditionReady reports whether current can be cloned.
	WorkspaceEnvironmentConditionReady = ConditionReady
	// WorkspaceEnvironmentConditionDraftReady reports whether draft can run
	// Environment edit processes.
	WorkspaceEnvironmentConditionDraftReady = "DraftReady"
)

// WorkspaceEnvironmentSpec defines the reusable image and home volume state.
type WorkspaceEnvironmentSpec struct {
	// image is the runner image string copied into each new Workspace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Image string `json:"image"`

	// storage configures current and draft home volumes.
	// +required
	Storage PersistentStorageSpec `json:"storage"`

	// editorIdleTimeout controls how long an idle editor Pod remains running.
	// Zero disables automatic suspension.
	// +optional
	EditorIdleTimeout *metav1.Duration `json:"editorIdleTimeout,omitempty"`

	// commit requests promotion of draft when incremented by rcctl env commit.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Commit int64 `json:"commit,omitempty"`
}

// WorkspaceEnvironmentStatus defines the observed state of WorkspaceEnvironment.
type WorkspaceEnvironmentStatus struct {
	// observedGeneration is the latest generation reflected by status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// currentRevision is the committed content revision cloned by new Workspaces.
	// +optional
	CurrentRevision int64 `json:"currentRevision,omitempty"`

	// currentImage is the exact image string associated with currentRevision.
	// +optional
	CurrentImage string `json:"currentImage,omitempty"`

	// currentVolumeClaimName is the PVC cloned by new Workspaces.
	// +optional
	CurrentVolumeClaimName string `json:"currentVolumeClaimName,omitempty"`

	// draftVolumeClaimName is the mutable draft PVC, when one exists.
	// +optional
	DraftVolumeClaimName string `json:"draftVolumeClaimName,omitempty"`

	// editorPodName is the active Environment editor Pod, when one exists.
	// +optional
	EditorPodName string `json:"editorPodName,omitempty"`

	// committedRequest is the latest spec.commit value successfully promoted.
	// +optional
	CommittedRequest int64 `json:"committedRequest,omitempty"`

	// conditions represent the current state of the WorkspaceEnvironment resource.
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
// +kubebuilder:resource:shortName=env
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Revision",type=integer,JSONPath=".status.currentRevision"
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=".status.currentImage"
// +kubebuilder:printcolumn:name="Current PVC",type=string,JSONPath=".status.currentVolumeClaimName"

// WorkspaceEnvironment is the Schema for the workspaceenvironments API
type WorkspaceEnvironment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WorkspaceEnvironment
	// +required
	Spec WorkspaceEnvironmentSpec `json:"spec"`

	// status defines the observed state of WorkspaceEnvironment
	// +optional
	Status WorkspaceEnvironmentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkspaceEnvironmentList contains a list of WorkspaceEnvironment
type WorkspaceEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WorkspaceEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &WorkspaceEnvironment{}, &WorkspaceEnvironmentList{})
		return nil
	})
}
