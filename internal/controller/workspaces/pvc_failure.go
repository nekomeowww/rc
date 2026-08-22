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
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *WorkspaceReconciler) persistentVolumeClaimFailure(ctx context.Context, claim *corev1.PersistentVolumeClaim) (string, string, error) {
	events := new(corev1.EventList)
	if err := r.List(ctx, events, client.InNamespace(claim.Namespace)); err != nil {
		return "", "", fmt.Errorf("list PersistentVolumeClaim Events: %w", err)
	}
	for _, v := range slices.Backward(events.Items) {
		event := &v
		if event.Type != corev1.EventTypeWarning || event.InvolvedObject.Kind != persistentVolumeClaimKind || event.InvolvedObject.Name != claim.Name {
			continue
		}
		if claim.UID != "" && event.InvolvedObject.UID != "" && event.InvolvedObject.UID != claim.UID {
			continue
		}
		return "StorageProvisioningFailed", fmt.Sprintf("PersistentVolumeClaim provisioning failed: %s: %s", event.Reason, event.Message), nil
	}

	return "", "", nil
}
