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
	"k8s.io/apimachinery/pkg/types"
)

// UsageReference identifies one resource that holds a Worktree or Repository
// volume. It mirrors the controller's hold set; it is not a lock.
type UsageReference struct {
	// kind is the holder's Kubernetes kind, for example Workspace or WorktreeExec.
	Kind string `json:"kind"`

	// name is the holder's name in the same namespace.
	Name string `json:"name"`

	// uid is the holder incarnation; a same-name replacement is a different holder.
	// +optional
	UID types.UID `json:"uid,omitempty"`

	// mode is the access mode: read or write on a Worktree; mount, clone or
	// write on a Repository.
	Mode string `json:"mode"`

	// since is when the holder was admitted.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
}

// RepositoryAccessStatus mirrors the Repository's access reservations.
type RepositoryAccessStatus struct {
	// mode is the shared mode of the current holders, empty when idle.
	// +optional
	Mode string `json:"mode,omitempty"`

	// holders lists the resources that reserve the parent volume.
	// +listType=atomic
	// +optional
	Holders []UsageReference `json:"holders,omitempty"`
}
