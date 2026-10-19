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

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// bindsOnFirstConsumer reports a Pending claim whose StorageClass binds it
// only after a Pod that uses it is scheduled, such as local-path. rc waits for
// Bound before creating that Pod, so such a claim never binds. A claim without
// a class name, or with a class that no longer exists, is not reported.
func bindsOnFirstConsumer(ctx context.Context, reader client.Reader, claim *corev1.PersistentVolumeClaim) (bool, error) {
	if claim.Status.Phase != corev1.ClaimPending || claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName == "" {
		return false, nil
	}
	class := new(storagev1.StorageClass)
	if err := reader.Get(ctx, client.ObjectKey{Name: *claim.Spec.StorageClassName}, class); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get StorageClass %s: %w", *claim.Spec.StorageClassName, err)
	}

	return class.VolumeBindingMode != nil && *class.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer, nil
}
