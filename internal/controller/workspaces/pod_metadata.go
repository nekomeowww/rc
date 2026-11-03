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

package workspaces

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	// workspacePodMetadataAnnotation records which spec.podMetadata keys rc
	// applied to the runtime Pod, so a key removed from the spec is also
	// removed from the Pod without touching keys that others set.
	workspacePodMetadataAnnotation = "workspaces.rc.ayaka.io/pod-metadata-keys"
	reasonReservedPodMetadataKey   = "ReservedPodMetadataKey"
	reasonInvalidPodMetadata       = "InvalidPodMetadata"
)

// appliedPodMetadata is the value of workspacePodMetadataAnnotation.
type appliedPodMetadata struct {
	Labels      []string `json:"labels,omitempty"`
	Annotations []string `json:"annotations,omitempty"`
}

// validateWorkspacePodMetadata repeats the CRD rule for reserved keys and checks
// the Kubernetes label and annotation syntax that the CRD does not express. It
// returns a Ready reason and message, or empty strings when the metadata is valid.
func validateWorkspacePodMetadata(metadata *workspacesv1alpha1.WorkspacePodMetadata) (string, string) {
	if metadata == nil {
		return "", ""
	}
	reserved := make([]string, 0)
	for _, key := range slices.Sorted(maps.Keys(metadata.Labels)) {
		if workspacesv1alpha1.IsReservedPodMetadataKey(key) {
			reserved = append(reserved, "label "+key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(metadata.Annotations)) {
		if workspacesv1alpha1.IsReservedPodMetadataKey(key) {
			reserved = append(reserved, "annotation "+key)
		}
	}
	if len(reserved) > 0 {
		return reasonReservedPodMetadataKey, fmt.Sprintf("spec.podMetadata uses keys reserved for rc (%s prefix): %s", workspacesv1alpha1.ReservedPodMetadataDomain, strings.Join(reserved, ", "))
	}
	path := field.NewPath("spec", "podMetadata")
	errs := metav1validation.ValidateLabels(metadata.Labels, path.Child("labels"))
	errs = append(errs, apivalidation.ValidateAnnotations(metadata.Annotations, path.Child("annotations"))...)
	if len(errs) > 0 {
		return reasonInvalidPodMetadata, errs.ToAggregate().Error()
	}

	return "", ""
}

// applyWorkspacePodMetadata merges spec.podMetadata into a runtime Pod and
// reports whether the Pod changed. Reserved rc keys are never written, so the
// labels and annotations that rc manages always win. Keys applied by an
// earlier spec and no longer requested are removed.
func applyWorkspacePodMetadata(pod *corev1.Pod, metadata *workspacesv1alpha1.WorkspacePodMetadata) bool {
	var desiredLabels, desiredAnnotations map[string]string
	if metadata != nil {
		desiredLabels = metadata.Labels
		desiredAnnotations = metadata.Annotations
	}
	previous := appliedPodMetadata{}
	if encoded := pod.Annotations[workspacePodMetadataAnnotation]; encoded != "" {
		// An unreadable record only loses stale-key cleanup; desired keys still apply.
		_ = json.Unmarshal([]byte(encoded), &previous)
	}

	labels, appliedLabels, labelsChanged := mergePodMetadataKeys(pod.Labels, previous.Labels, desiredLabels)
	annotations, appliedAnnotations, annotationsChanged := mergePodMetadataKeys(pod.Annotations, previous.Annotations, desiredAnnotations)
	record := ""
	if len(appliedLabels) > 0 || len(appliedAnnotations) > 0 {
		encoded, err := json.Marshal(appliedPodMetadata{Labels: appliedLabels, Annotations: appliedAnnotations})
		if err == nil {
			record = string(encoded)
		}
	}
	if annotations[workspacePodMetadataAnnotation] != record {
		annotationsChanged = true
		if record == "" {
			delete(annotations, workspacePodMetadataAnnotation)
		} else {
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations[workspacePodMetadataAnnotation] = record
		}
	}
	if labelsChanged {
		pod.Labels = labels
	}
	if annotationsChanged {
		pod.Annotations = annotations
	}

	return labelsChanged || annotationsChanged
}

// mergePodMetadataKeys returns a copy of current with stale previously applied
// keys removed and desired keys set, the sorted keys now applied, and whether
// the map changed.
func mergePodMetadataKeys(current map[string]string, previous []string, desired map[string]string) (map[string]string, []string, bool) {
	merged := maps.Clone(current)
	changed := false
	for _, key := range previous {
		if workspacesv1alpha1.IsReservedPodMetadataKey(key) {
			continue
		}
		if _, keep := desired[key]; keep {
			continue
		}
		if _, exists := merged[key]; exists {
			delete(merged, key)
			changed = true
		}
	}
	applied := make([]string, 0, len(desired))
	for _, key := range slices.Sorted(maps.Keys(desired)) {
		if workspacesv1alpha1.IsReservedPodMetadataKey(key) {
			continue
		}
		applied = append(applied, key)
		if value, exists := merged[key]; exists && value == desired[key] {
			continue
		}
		if merged == nil {
			merged = make(map[string]string, len(desired))
		}
		merged[key] = desired[key]
		changed = true
	}

	return merged, applied, changed
}

// reconcileRuntimePodMetadata updates labels and annotations on a running
// runtime Pod in place. Pod metadata is mutable, so this never restarts it.
func (r *WorkspaceReconciler) reconcileRuntimePodMetadata(ctx context.Context, workspace *workspacesv1alpha1.Workspace, pod *corev1.Pod) error {
	before := pod.DeepCopy()
	if !applyWorkspacePodMetadata(pod, workspace.Spec.PodMetadata) {
		return nil
	}
	if err := r.Patch(ctx, pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("update Workspace runtime Pod metadata: %w", err)
	}
	logf.FromContext(ctx).Info("Updated Workspace runtime Pod metadata", "name", pod.Name)

	return nil
}
