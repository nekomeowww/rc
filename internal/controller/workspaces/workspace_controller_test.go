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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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

func TestWorkspaceReconcileClonesEnvironmentAndCreatesRuntime(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
	requirements.NoError(coordinationv1.AddToScheme(scheme), "register coordination API types")
	requirements.NoError(rbacv1.AddToScheme(scheme), "register RBAC API types")
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")

	environment := &workspacesv1alpha1.WorkspaceEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "prepared", Namespace: testNamespace},
		Spec: workspacesv1alpha1.WorkspaceEnvironmentSpec{
			Image: "ghcr.io/example/workspace:latest",
			Storage: workspacesv1alpha1.PersistentStorageSpec{
				StorageClassName: testStorageClass,
				Size:             resource.MustParse("20Gi"),
			},
		},
		Status: workspacesv1alpha1.WorkspaceEnvironmentStatus{
			CurrentRevision:        4,
			CurrentImage:           "ghcr.io/example/workspace:latest",
			CurrentVolumeClaimName: "prepared-current-4",
			Conditions: []metav1.Condition{{
				Type: workspacesv1alpha1.WorkspaceEnvironmentConditionReady, Status: metav1.ConditionTrue, Reason: "EnvironmentReady",
			}},
		},
	}
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: "rc-main", Namespace: testNamespace},
		Spec: repositoriesv1alpha1.WorktreeSpec{
			RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: "rc"},
		},
		Status: repositoriesv1alpha1.WorktreeStatus{
			VolumeClaimName: "rc-main",
			WorktreePath:    "/repository/worktree",
			Conditions: []metav1.Condition{{
				Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue, Reason: "WorktreeReady",
			}},
		},
	}
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testNamespace, UID: types.UID("workspace-uid")},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState:   workspacesv1alpha1.WorkspaceDesiredStateRunning,
			EnvironmentRef: &workspacesv1alpha1.LocalReference{Name: environment.Name},
			Mounts: []workspacesv1alpha1.WorkspaceMount{{
				Name: "rc", Path: "rc", WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name},
			}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(workspace, environment, worktree, &workspacesv1alpha1.AgentProcess{}, &corev1.PersistentVolumeClaim{}, &corev1.Pod{}).
		WithObjects(environment, worktree, workspace).
		Build()
	reconciler := &WorkspaceReconciler{Client: kubeClient, Scheme: scheme, RunnerImage: "ghcr.io/example/rc/runner:test"}
	key := types.NamespacedName{Name: workspace.Name, Namespace: workspace.Namespace}

	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "create Workspace home clone")
	home := new(corev1.PersistentVolumeClaim)
	requirements.NoError(kubeClient.Get(ctx, key, home), "get Workspace home PVC")
	requirements.NotNil(home.Spec.DataSource, "Environment Workspace clones current PVC")
	assertions.Equal(environment.Status.CurrentVolumeClaimName, home.Spec.DataSource.Name, "clone the committed revision")

	home.Status.Phase = corev1.ClaimBound
	requirements.NoError(kubeClient.Status().Update(ctx, home), "mark Workspace home bound")
	wrongRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: defaultWorkspaceServiceAccount, Namespace: workspace.Namespace}, Rules: []rbacv1.PolicyRule{{Resources: []string{"wrong"}, Verbs: []string{"get"}}}}
	requirements.NoError(kubeClient.Create(ctx, wrongRole), "seed drifted shared Role")
	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "create Workspace runtime")

	pod := new(corev1.Pod)
	requirements.NoError(kubeClient.Get(ctx, key, pod), "get Workspace runtime Pod")
	requirements.Len(pod.Spec.Containers, 1, "runtime Pod has one supervisor container")
	requirements.NotNil(pod.Spec.Containers[0].SecurityContext, "runtime container has a SecurityContext")
	requirements.NotNil(pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation, "runtime container declares privilege escalation policy")
	assertions.Equal(environment.Status.CurrentImage, pod.Spec.Containers[0].Image, "use captured Environment image")
	assertions.Equal([]string{runtimeContainerName, runtimeServeArgument}, pod.Spec.Containers[0].Command, "run the supervisor")
	assertions.Equal("rc-workspace", pod.Spec.ServiceAccountName, "inject default namespaced ServiceAccount")
	assertions.Equal(workspaceRuntimePolicyVersion, pod.Annotations[workspaceRuntimePolicyAnnotation], "record the restricted runtime policy")
	assertions.False(*pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation, "prevent Agent Processes from gaining root")
	requirements.NotNil(pod.Spec.Containers[0].SecurityContext.Capabilities, "runtime container declares capabilities")
	assertions.Equal([]corev1.Capability{allLinuxCapabilities}, pod.Spec.Containers[0].SecurityContext.Capabilities.Drop, "drop every runtime capability")
	assertions.Empty(pod.Spec.InitContainers, "normal Workspace does not inject sudoers")
	for _, volume := range pod.Spec.Volumes {
		assertions.NotEqual(environmentSudoersVolumeName, volume.Name, "normal Workspace has no sudoers volume")
	}
	assertions.Equal("/home/agent", pod.Spec.Containers[0].VolumeMounts[0].MountPath, "mount persistent home")
	assertions.Equal("/workspace/rc", pod.Spec.Containers[0].VolumeMounts[1].MountPath, "mount selected Worktree")
	assertions.Equal("worktree", pod.Spec.Containers[0].VolumeMounts[1].SubPath, "mount native Git worktree directory")
	assertions.True(metav1.IsControlledBy(pod, workspace), "Workspace owns runtime Pod")

	serviceAccount := new(corev1.ServiceAccount)
	requirements.NoError(kubeClient.Get(ctx, types.NamespacedName{Name: "rc-workspace", Namespace: workspace.Namespace}, serviceAccount), "get shared ServiceAccount")
	role := new(rbacv1.Role)
	requirements.NoError(kubeClient.Get(ctx, types.NamespacedName{Name: "rc-workspace", Namespace: workspace.Namespace}, role), "get shared Role")
	assertions.Contains(role.Rules[0].Resources, "agentprocesses", "nested rcctl can manage process resources")
	leases := new(coordinationv1.LeaseList)
	requirements.NoError(kubeClient.List(ctx, leases, client.InNamespace(workspace.Namespace)), "list Worktree write Leases")
	requirements.Len(leases.Items, 1, "claim each writable Worktree atomically")
	requirements.NotNil(leases.Items[0].Spec.HolderIdentity, "Lease records holder")
	assertions.Equal(string(workspace.UID), *leases.Items[0].Spec.HolderIdentity, "Workspace UID holds Worktree write Lease")

	persisted := new(workspacesv1alpha1.Workspace)
	requirements.NoError(kubeClient.Get(ctx, key, persisted), "get reconciled Workspace")
	assertions.Equal(int64(4), persisted.Status.SourceEnvironmentRevision, "record cloned revision")
	assertions.Equal(environment.Status.CurrentImage, persisted.Status.RuntimeImage, "snapshot exact image string")
	assertions.Equal(workspace.Name, persisted.Status.HomeVolumeClaimName, "publish home PVC")
	ready := meta.FindStatusCondition(persisted.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
	requirements.NotNil(ready, "publish Ready condition")
	assertions.Equal(metav1.ConditionFalse, ready.Status, "pending Pod is not ready")
	activeProcess := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: workspace.Namespace},
		Spec: workspacesv1alpha1.AgentProcessSpec{
			TargetRef: workspacesv1alpha1.AgentProcessTargetReference{Kind: workspacesv1alpha1.AgentProcessTargetWorkspace, Name: workspace.Name},
			Command:   []string{"sleep", "60"},
		},
		Status: workspacesv1alpha1.AgentProcessStatus{Phase: workspacesv1alpha1.AgentProcessPhaseRunning},
	}
	requirements.NoError(kubeClient.Create(ctx, activeProcess), "create active Agent Process")
	requirements.NoError(kubeClient.Status().Update(ctx, activeProcess), "mark Agent Process running")
	secondWorktree := worktree.DeepCopy()
	secondWorktree.Name = "rc-other"
	secondWorktree.UID = types.UID("second-worktree-uid")
	secondWorktree.ResourceVersion = ""
	requirements.NoError(kubeClient.Create(ctx, secondWorktree), "create second Worktree")
	requirements.NoError(kubeClient.Status().Update(ctx, secondWorktree), "publish second Worktree status")
	currentWorkspace := new(workspacesv1alpha1.Workspace)
	requirements.NoError(kubeClient.Get(ctx, key, currentWorkspace), "get Workspace before topology edit")
	originalMounts := append([]workspacesv1alpha1.WorkspaceMount(nil), currentWorkspace.Spec.Mounts...)
	currentWorkspace.Spec.Mounts = append(currentWorkspace.Spec.Mounts, workspacesv1alpha1.WorkspaceMount{Name: "other", Path: "other", WorktreeRef: &workspacesv1alpha1.LocalReference{Name: secondWorktree.Name}})
	requirements.NoError(kubeClient.Update(ctx, currentWorkspace), "request topology edit")
	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "reject topology edit while process is active")
	requirements.NoError(kubeClient.Get(ctx, key, pod), "retain runtime Pod for blocked topology edit")
	blockedLeases := new(coordinationv1.LeaseList)
	requirements.NoError(kubeClient.List(ctx, blockedLeases, client.InNamespace(workspace.Namespace)), "list claims after blocked topology edit")
	assertions.Len(blockedLeases.Items, 1, "do not acquire desired claims before topology replacement")
	requirements.NoError(kubeClient.Get(ctx, key, currentWorkspace), "get Workspace to revert topology")
	currentWorkspace.Spec.Mounts = originalMounts
	requirements.NoError(kubeClient.Update(ctx, currentWorkspace), "revert blocked topology edit")
	requirements.NoError(kubeClient.Delete(ctx, activeProcess), "remove active Agent Process")
	foreignHolder := "another-workspace-uid"
	leases.Items[0].Spec.HolderIdentity = &foreignHolder
	requirements.NoError(kubeClient.Update(ctx, &leases.Items[0]), "simulate Worktree Lease loss")
	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "reconcile lost Worktree Lease")
	removedPod := new(corev1.Pod)
	assertions.Error(kubeClient.Get(ctx, key, removedPod), "stop runtime Pod immediately after losing write claim")
}

func TestGeneratedWorkspaceSuspendsAfterItsProcessFinishes(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
	requirements.NoError(coordinationv1.AddToScheme(scheme), "register coordination API types")
	requirements.NoError(rbacv1.AddToScheme(scheme), "register RBAC API types")
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	now := metav1.NewTime(time.Now().Add(-time.Second))
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "generated", Namespace: testNamespace, UID: types.UID("workspace-uid")},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState: workspacesv1alpha1.WorkspaceDesiredStateRunning, Generated: true, Image: testRuntimeImage,
			Storage: &workspacesv1alpha1.PersistentStorageSpec{StorageClassName: testStorageClass, Size: resource.MustParse("20Gi")},
		},
		Status: workspacesv1alpha1.WorkspaceStatus{RuntimeImage: testRuntimeImage, Conditions: []metav1.Condition{{
			Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, Reason: "WorkspaceReady", LastTransitionTime: now,
		}}},
	}
	home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace, OwnerReferences: []metav1.OwnerReference{{UID: workspace.UID, Controller: boolPointer(true)}}}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	process := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "codex-finished", Namespace: workspace.Namespace},
		Spec:       workspacesv1alpha1.AgentProcessSpec{TargetRef: workspacesv1alpha1.AgentProcessTargetReference{Kind: workspacesv1alpha1.AgentProcessTargetWorkspace, Name: workspace.Name}, Command: []string{testTrueValue}},
		Status:     workspacesv1alpha1.AgentProcessStatus{Phase: workspacesv1alpha1.AgentProcessPhaseSucceeded, CompletedAt: &now},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace, process, home).WithObjects(workspace, home, process).Build()
	reconciler := &WorkspaceReconciler{Client: kubeClient, Scheme: scheme}
	_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: workspace.Name, Namespace: workspace.Namespace}})
	requirements.NoError(err, "reconcile completed generated Workspace")
	persisted := new(workspacesv1alpha1.Workspace)
	requirements.NoError(kubeClient.Get(context.Background(), client.ObjectKeyFromObject(workspace), persisted), "get generated Workspace")
	requirements.Equal(workspacesv1alpha1.WorkspaceDesiredStateSuspended, persisted.Spec.DesiredState, "release runtime compute after process completion")
}

func TestWorkspaceNeverMutatesUnownedRuntimePod(t *testing.T) {
	t.Parallel()

	for _, desiredState := range []workspacesv1alpha1.WorkspaceDesiredState{
		workspacesv1alpha1.WorkspaceDesiredStateRunning,
		workspacesv1alpha1.WorkspaceDesiredStateSuspended,
	} {
		t.Run(string(desiredState), func(t *testing.T) {
			t.Parallel()
			requirements := require.New(t)
			ctx := context.Background()
			scheme := runtime.NewScheme()
			requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
			requirements.NoError(coordinationv1.AddToScheme(scheme), "register coordination API types")
			requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
			automount := false
			workspace := &workspacesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod-conflict-" + string(desiredState), Namespace: testNamespace,
					UID: types.UID("workspace-uid"), Finalizers: []string{workspaceFinalizer},
				},
				Spec: workspacesv1alpha1.WorkspaceSpec{
					DesiredState: desiredState, Image: testRuntimeImage,
					Storage: &workspacesv1alpha1.PersistentStorageSpec{
						StorageClassName: testStorageClass, Size: resource.MustParse("20Gi"),
					},
					AutomountServiceAccountToken: &automount,
				},
				Status: workspacesv1alpha1.WorkspaceStatus{RuntimeImage: testRuntimeImage},
			}
			home := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: workspace.Name, Namespace: workspace.Namespace,
					OwnerReferences: []metav1.OwnerReference{{UID: workspace.UID, Controller: boolPointer(true)}},
				},
				Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
			}
			foreignPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: workspace.Name, Namespace: workspace.Namespace, UID: types.UID("foreign-pod-uid"),
			}}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(workspace, home, foreignPod, &workspacesv1alpha1.AgentProcess{}).
				WithObjects(workspace, home, foreignPod).
				Build()
			reconciler := &WorkspaceReconciler{Client: kubeClient, Scheme: scheme}
			key := client.ObjectKeyFromObject(workspace)

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			requirements.NoError(err, "report runtime Pod ownership conflict")
			persistedPod := new(corev1.Pod)
			requirements.NoError(kubeClient.Get(ctx, key, persistedPod), "retain unowned same-name Pod")
			requirements.Equal(foreignPod.UID, persistedPod.UID, "never replace the unowned Pod")
			persistedWorkspace := new(workspacesv1alpha1.Workspace)
			requirements.NoError(kubeClient.Get(ctx, key, persistedWorkspace), "get conflicted Workspace")
			condition := meta.FindStatusCondition(persistedWorkspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
			requirements.NotNil(condition, "publish conflict condition")
			requirements.Equal("RuntimePodConflict", condition.Reason, "identify ownership conflict")
		})
	}
}

func TestWorkspaceDeletionLeavesUnownedSameNamePod(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	ctx := context.Background()
	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
	requirements.NoError(coordinationv1.AddToScheme(scheme), "register coordination API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{
		Name: "delete-pod-conflict", Namespace: testNamespace, UID: types.UID("workspace-uid"),
		Finalizers: []string{workspaceFinalizer},
	}}
	foreignPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: workspace.Name, Namespace: workspace.Namespace, UID: types.UID("foreign-pod-uid"),
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(workspace, foreignPod, &workspacesv1alpha1.AgentProcess{}).
		WithObjects(workspace, foreignPod).
		Build()
	reconciler := &WorkspaceReconciler{Client: kubeClient, Scheme: scheme}
	key := client.ObjectKeyFromObject(workspace)

	requirements.NoError(kubeClient.Delete(ctx, workspace), "request Workspace deletion")
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "finalize Workspace without mutating unowned Pod")
	requirements.NoError(kubeClient.Get(ctx, key, new(corev1.Pod)), "retain unowned same-name Pod")
	err = kubeClient.Get(ctx, key, new(workspacesv1alpha1.Workspace))
	requirements.Error(err, "delete Workspace after cleaning only its owned resources")
}
