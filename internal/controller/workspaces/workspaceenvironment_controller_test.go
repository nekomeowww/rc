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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func TestWorkspaceEnvironmentReconcileCreatesCurrentVolume(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)

	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")

	environment := &workspacesv1alpha1.WorkspaceEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "node-rust", Namespace: testNamespace, UID: types.UID("environment-uid")},
		Spec: workspacesv1alpha1.WorkspaceEnvironmentSpec{
			Image: "ghcr.io/example/workspace:node-rust",
			Storage: workspacesv1alpha1.PersistentStorageSpec{
				StorageClassName: testStorageClass,
				Size:             resource.MustParse("20Gi"),
			},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(environment, &corev1.PersistentVolumeClaim{}).
		WithObjects(environment).
		Build()
	reconciler := &WorkspaceEnvironmentReconciler{Client: kubeClient, Scheme: scheme}
	key := types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}

	_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "reconcile a new WorkspaceEnvironment")

	claim := new(corev1.PersistentVolumeClaim)
	requirements.NoError(kubeClient.Get(context.Background(), types.NamespacedName{Name: "node-rust-current-1", Namespace: environment.Namespace}, claim), "get current PVC")
	assertions.Nil(claim.Spec.DataSource, "initial current volume is blank")
	assertions.Equal(testStorageClass, *claim.Spec.StorageClassName, "preserve requested StorageClass")
	assertions.Equal(corev1.PersistentVolumeFilesystem, *claim.Spec.VolumeMode, "home is a filesystem")
	assertions.Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, claim.Spec.AccessModes, "default to a single-writer home")
	assertions.True(metav1.IsControlledBy(claim, environment), "Environment owns current PVC")

	persisted := new(workspacesv1alpha1.WorkspaceEnvironment)
	requirements.NoError(kubeClient.Get(context.Background(), key, persisted), "get reconciled Environment")
	assertions.Equal(int64(1), persisted.Status.CurrentRevision, "establish the first revision")
	assertions.Equal(environment.Spec.Image, persisted.Status.CurrentImage, "capture exact image string")
	assertions.Equal(claim.Name, persisted.Status.CurrentVolumeClaimName, "publish clone source")
	ready := meta.FindStatusCondition(persisted.Status.Conditions, workspacesv1alpha1.WorkspaceEnvironmentConditionReady)
	requirements.NotNil(ready, "publish Ready condition")
	assertions.Equal(metav1.ConditionFalse, ready.Status, "unbound PVC is not ready")
}

func TestEnvironmentEditorPodSupportsPasswordlessSudo(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	environment := &workspacesv1alpha1.WorkspaceEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "sudo", Namespace: testNamespace},
		Spec: workspacesv1alpha1.WorkspaceEnvironmentSpec{
			Image: "ghcr.io/example/workspace:sudo",
		},
	}

	pod := environmentEditorPod(environment, "sudo-draft-1")
	requirements.Len(pod.Spec.InitContainers, 1, "editor Pod has one sudoers setup container")
	setup := pod.Spec.InitContainers[0]
	assertions.Equal(environmentSudoersContainerName, setup.Name, "use a dedicated sudoers setup container")
	assertions.Equal(environment.Spec.Image, setup.Image, "configure sudoers with the Environment image")
	assertions.Equal([]string{"/bin/sh", "-ec"}, setup.Command, "run the fixed sudoers setup script")
	requirements.Len(setup.Args, 1, "sudoers setup has one fixed script")
	assertions.Contains(setup.Args[0], environmentSudoersRule, "install the agent passwordless sudo rule")
	requirements.NotNil(setup.SecurityContext, "sudoers setup has a SecurityContext")
	requirements.NotNil(setup.SecurityContext.RunAsUser, "sudoers setup declares its user")
	requirements.NotNil(setup.SecurityContext.RunAsNonRoot, "sudoers setup overrides the Pod non-root policy")
	requirements.NotNil(setup.SecurityContext.AllowPrivilegeEscalation, "sudoers setup declares its escalation policy")
	assertions.Zero(*setup.SecurityContext.RunAsUser, "sudoers setup writes the root-owned file as root")
	assertions.False(*setup.SecurityContext.RunAsNonRoot, "allow the bounded setup container to run as root")
	assertions.False(*setup.SecurityContext.AllowPrivilegeEscalation, "setup does not need to gain more privileges")
	requirements.NotNil(setup.SecurityContext.Capabilities, "sudoers setup declares capabilities")
	assertions.Equal([]corev1.Capability{allLinuxCapabilities}, setup.SecurityContext.Capabilities.Drop, "drop all setup capabilities")
	requirements.Len(setup.VolumeMounts, 1, "sudoers setup mounts only its output volume")
	assertions.Equal(environmentSudoersVolumeName, setup.VolumeMounts[0].Name, "write into the editor-only sudoers volume")

	requirements.Len(pod.Spec.Containers, 1, "editor Pod has one supervisor container")
	requirements.NotNil(pod.Spec.Containers[0].SecurityContext, "editor container has a SecurityContext")
	requirements.NotNil(pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation, "editor container declares privilege escalation policy")
	assertions.True(*pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation, "allow passwordless sudo to execute its setuid helper")
	assertions.Nil(pod.Spec.Containers[0].SecurityContext.Capabilities, "retain the runtime default capability set for useful container root")
	requirements.Len(pod.Spec.Containers[0].VolumeMounts, 3, "editor mounts home, runtime state, and sudoers")
	assertions.Equal(environmentSudoersVolumeName, pod.Spec.Containers[0].VolumeMounts[2].Name, "inject sudoers only into the editor")
	assertions.True(pod.Spec.Containers[0].VolumeMounts[2].ReadOnly, "keep injected sudoers immutable in the editor")
	requirements.Len(pod.Spec.Volumes, 3, "editor Pod has an isolated sudoers volume")
	assertions.Equal(environmentSudoersVolumeName, pod.Spec.Volumes[2].Name, "name the editor-only sudoers volume")
	requirements.NotNil(pod.Spec.Volumes[2].EmptyDir, "discard injected sudoers with the editor Pod")
}

func TestWorkspaceEnvironmentReconcileCommitsIdleDraft(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	environment := &workspacesv1alpha1.WorkspaceEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python", Namespace: testNamespace, UID: types.UID("environment-uid")},
		Spec: workspacesv1alpha1.WorkspaceEnvironmentSpec{
			Image: "ghcr.io/example/workspace:python-v2", Commit: 1,
			Storage: workspacesv1alpha1.PersistentStorageSpec{StorageClassName: testStorageClass, Size: resource.MustParse("10Gi")},
		},
		Status: workspacesv1alpha1.WorkspaceEnvironmentStatus{
			CurrentRevision: 1, CurrentImage: "ghcr.io/example/workspace:python-v1",
			CurrentVolumeClaimName: "python-current-1", DraftVolumeClaimName: "python-draft-2",
			EditorPodName: "python-editor",
		},
	}
	current := environmentVolumeClaim(environment, "python-current-1", "")
	draft := environmentVolumeClaim(environment, "python-draft-2", current.Name)
	editor := environmentEditorPod(environment, draft.Name)
	requirements.NoError(controllerutil.SetControllerReference(environment, current, scheme), "own current PVC")
	requirements.NoError(controllerutil.SetControllerReference(environment, draft, scheme), "own draft PVC")
	requirements.NoError(controllerutil.SetControllerReference(environment, editor, scheme), "own editor Pod")
	current.Status.Phase = corev1.ClaimBound
	draft.Status.Phase = corev1.ClaimBound
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(environment, current, draft, editor).
		WithObjects(environment, current, draft, editor).
		Build()
	reconciler := &WorkspaceEnvironmentReconciler{Client: kubeClient, Scheme: scheme}
	key := types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}

	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "stop editor before commit")
	deletedEditor := new(corev1.Pod)
	assertions.Error(kubeClient.Get(ctx, types.NamespacedName{Name: editor.Name, Namespace: editor.Namespace}, deletedEditor), "editor Pod is removed before promotion")

	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "promote idle draft")
	persisted := new(workspacesv1alpha1.WorkspaceEnvironment)
	requirements.NoError(kubeClient.Get(ctx, key, persisted), "get committed Environment")
	assertions.Equal(int64(2), persisted.Status.CurrentRevision, "advance content revision")
	assertions.Equal(draft.Name, persisted.Status.CurrentVolumeClaimName, "promote draft PVC")
	assertions.Equal(environment.Spec.Image, persisted.Status.CurrentImage, "commit exact image string")
	assertions.Equal(int64(1), persisted.Status.CommittedRequest, "acknowledge commit request")
	assertions.Empty(persisted.Status.DraftVolumeClaimName, "clear promoted draft")
	assertions.Empty(persisted.Status.EditorPodName, "clear stopped editor")
	oldCurrent := new(corev1.PersistentVolumeClaim)
	assertions.Error(kubeClient.Get(ctx, types.NamespacedName{Name: current.Name, Namespace: current.Namespace}, oldCurrent), "delete previous current PVC")
}

func TestWorkspaceEnvironmentImageOnlyAdvancesOnCommit(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	environment := &workspacesv1alpha1.WorkspaceEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "image-snapshot", Namespace: testNamespace, UID: types.UID("environment-uid")},
		Spec:       workspacesv1alpha1.WorkspaceEnvironmentSpec{Image: "workspace:new", Storage: workspacesv1alpha1.PersistentStorageSpec{StorageClassName: testStorageClass, Size: resource.MustParse("10Gi")}},
		Status:     workspacesv1alpha1.WorkspaceEnvironmentStatus{CurrentRevision: 1, CurrentImage: "workspace:committed", CurrentVolumeClaimName: "image-snapshot-current-1"},
	}
	claim := environmentVolumeClaim(environment, environment.Status.CurrentVolumeClaimName, "")
	requirements.NoError(controllerutil.SetControllerReference(environment, claim, scheme), "own current PVC")
	claim.Status.Phase = corev1.ClaimBound
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(environment, claim).WithObjects(environment, claim).Build()
	reconciler := &WorkspaceEnvironmentReconciler{Client: kubeClient, Scheme: scheme}
	key := types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}

	_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "reconcile edited image without commit")
	persisted := new(workspacesv1alpha1.WorkspaceEnvironment)
	requirements.NoError(kubeClient.Get(context.Background(), key, persisted), "get reconciled Environment")
	requirements.Equal("workspace:committed", persisted.Status.CurrentImage, "retain committed image until commit")
}
