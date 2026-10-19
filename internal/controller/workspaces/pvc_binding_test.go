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
	"testing"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func TestWorkspaceHomeReportsWaitForFirstConsumer(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		mode      storagev1.VolumeBindingMode
		wantState string
	}{
		{
			name:      "WaitForFirstConsumerReportsUnsupported",
			mode:      storagev1.VolumeBindingWaitForFirstConsumer,
			wantState: workspacesv1alpha1.ReasonWaitForFirstConsumerUnsupported,
		},
		{
			name:      "ImmediateWaitsForBound",
			mode:      storagev1.VolumeBindingImmediate,
			wantState: "Provisioning",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requirements := require.New(t)
			ctx := context.Background()
			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{
				corev1.AddToScheme,
				coordinationv1.AddToScheme,
				rbacv1.AddToScheme,
				storagev1.AddToScheme,
				repositoriesv1alpha1.AddToScheme,
				workspacesv1alpha1.AddToScheme,
			} {
				requirements.NoError(add(scheme))
			}
			mode := test.mode
			class := &storagev1.StorageClass{
				ObjectMeta:        metav1.ObjectMeta{Name: "local-path"},
				Provisioner:       "rancher.io/local-path",
				VolumeBindingMode: &mode,
			}
			workspace := &workspacesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{Name: "local-home", Namespace: testNamespace, UID: types.UID("local-home-uid")},
				Spec: workspacesv1alpha1.WorkspaceSpec{
					Image: testRuntimeImage,
					Storage: &workspacesv1alpha1.PersistentStorageSpec{
						StorageClassName: class.Name,
						Size:             resource.MustParse("5Gi"),
					},
				},
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(workspace, &corev1.PersistentVolumeClaim{}).
				WithObjects(class, workspace).
				WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).
				Build()
			reconciler := &WorkspaceReconciler{Client: kubeClient, APIReader: kubeClient, Scheme: scheme, RunnerImage: testRunnerImage}
			key := client.ObjectKeyFromObject(workspace)
			reconcileWorkspace := func() {
				t.Helper()
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				requirements.NoError(err)
			}

			reconcileWorkspace()
			home := new(corev1.PersistentVolumeClaim)
			requirements.NoError(kubeClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "workspace-" + workspace.Name + "-home"}, home))
			// The API server publishes Pending for a new claim; the fake client does not.
			home.Status.Phase = corev1.ClaimPending
			requirements.NoError(kubeClient.Status().Update(ctx, home))
			for range 3 {
				reconcileWorkspace()
			}

			err := kubeClient.Get(ctx, key, new(corev1.Pod))
			requirements.True(apierrors.IsNotFound(err), "create the runtime only after the home volume binds")
			requirements.NoError(kubeClient.Get(ctx, key, workspace))
			storage := meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.ConditionStorageReady)
			requirements.NotNil(storage)
			requirements.Equal(metav1.ConditionFalse, storage.Status)
			requirements.Equal(test.wantState, storage.Reason)
		})
	}
}
