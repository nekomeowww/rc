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
)

const (
	// ConditionReady reports whether a Workspace resource can serve new work.
	ConditionReady = "Ready"
	// ConditionOutdated reports whether a Workspace was cloned from an older
	// Environment revision or image value.
	ConditionOutdated = "Outdated"
)

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
