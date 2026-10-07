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
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
)

// ROOT CAUSE: a terminal owned runtime is never Ready, but the original
// controller treats all non-ready Pods as still starting and keeps their UID.
// A bound execution must also become Lost without ever starting on a new UID.
func TestWorkspaceTerminalRuntimeRecovery(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		for _, active := range []bool{false, true} {
			name := string(phase) + "/idle"
			if active {
				name = string(phase) + "/active"
			}
			t.Run(name, func(t *testing.T) {
				fixture := newRuntimeRecoveryFixture(t)
				ctx, kubeClient, workspace, pod := fixture.ctx, fixture.client, fixture.workspace, fixture.pod
				key := client.ObjectKeyFromObject(workspace)
				pod.Status.Phase = phase
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: runtimeContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}}}}
				require.NoError(t, kubeClient.Status().Update(ctx, pod))
				if active {
					process := &workspacesv1alpha1.WorkspaceExec{
						ObjectMeta: metav1.ObjectMeta{Name: "bound-execution", Namespace: workspace.Namespace, UID: "exec-uid"},
						Spec:       workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name}, Command: []string{"cat"}},
						Status:     workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseRunning, RuntimePodName: pod.Name, RuntimePodUID: string(pod.UID)},
					}
					require.NoError(t, kubeClient.Create(ctx, process))
				}
				fixture.reconcile(t)
				condition := meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
				require.NotNil(t, condition)
				reason := "RuntimeFailed"
				if phase == corev1.PodSucceeded {
					reason = "RuntimeCompleted"
				}
				require.Equal(t, reason, condition.Reason, "terminal runtime must leave Starting with its actual exit diagnosis")
				require.Equal(t, metav1.ConditionFalse, condition.Status)
				require.True(t, meta.IsStatusConditionTrue(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionDegraded))
				require.Contains(t, condition.Message, "exitCode=0")
				if active {
					fixture.restart()
					fixture.reconcile(t)
					require.NoError(t, kubeClient.Get(ctx, key, new(corev1.Pod)), "wait for bound executions before deleting")
					processRuntime := &recordingProcessRuntime{}
					execReconciler := &WorkspaceExecReconciler{Client: kubeClient, Runtime: processRuntime}
					execKey := client.ObjectKey{Namespace: workspace.Namespace, Name: "bound-execution"}
					for range 2 {
						_, err := execReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: execKey})
						require.NoError(t, err)
					}
					execution := new(workspacesv1alpha1.WorkspaceExec)
					require.NoError(t, kubeClient.Get(ctx, execKey, execution))
					require.Equal(t, workspacesv1alpha1.WorkspaceExecPhaseLost, execution.Status.Phase)
					require.Nil(t, execution.Status.ExitCode, "runtime exit is not the execution exit code")
					fixture.reconcile(t)
					require.True(t, apierrors.IsNotFound(kubeClient.Get(ctx, key, new(corev1.Pod))))
					fixture.reconcile(t)
					replacement := fixture.setPodReady(t, "replacement")
					fixture.reconcile(t)
					_, err := execReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: execKey})
					require.NoError(t, err)
					require.Empty(t, processRuntime.startedRequest.ID, "Lost execution never reruns on a replacement")
					require.NotEqual(t, pod.UID, replacement.UID)
				} else {
					require.True(t, apierrors.IsNotFound(kubeClient.Get(ctx, key, new(corev1.Pod))), "delete terminal runtime after recording its exit")
					fixture.reconcile(t)
					require.NoError(t, kubeClient.Get(ctx, key, new(corev1.Pod)), "recreate compute using retained home")
				}

			})
		}
	}
}

func TestWorkspaceExecTerminalPodBecomesLost(t *testing.T) {
	for _, phase := range []workspacesv1alpha1.WorkspaceExecPhase{workspacesv1alpha1.WorkspaceExecPhasePending, workspacesv1alpha1.WorkspaceExecPhaseStarting, workspacesv1alpha1.WorkspaceExecPhaseRunning} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dead", Namespace: testNamespace, UID: "dead-uid"}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
			process := &workspacesv1alpha1.WorkspaceExec{
				ObjectMeta: metav1.ObjectMeta{Name: "bound", Namespace: testNamespace, UID: "process-uid", Finalizers: []string{executionFinalizer}},
				Spec:       workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: "unready-workspace"}},
				Status:     workspacesv1alpha1.WorkspaceExecStatus{Phase: phase, RuntimePodName: pod.Name, RuntimePodUID: string(pod.UID)},
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(process, pod).WithObjects(process, pod).Build()
			processRuntime := &recordingProcessRuntime{}
			reconciler := &WorkspaceExecReconciler{Client: kubeClient, Runtime: processRuntime}
			request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)}
			_, err := reconciler.Reconcile(ctx, request)
			require.NoError(t, err)
			require.NoError(t, kubeClient.Get(ctx, request.NamespacedName, process))
			require.Equal(t, workspacesv1alpha1.WorkspaceExecPhaseLost, process.Status.Phase)
			require.Empty(t, processRuntime.startedRequest.ID)
			require.Nil(t, process.Status.ExitCode, "runtime exit is not a known child exit")
			// An RPC that completed after the loss observation cannot resurrect it.
			require.NoError(t, reconciler.applyRuntimeState(ctx, request.NamespacedName, &resolvedProcessTarget{}, processruntime.State{Phase: "Running"}))
			require.NoError(t, kubeClient.Get(ctx, request.NamespacedName, process))
			require.Equal(t, workspacesv1alpha1.WorkspaceExecPhaseLost, process.Status.Phase)
		})
	}
}

// runtimeRecoveryFixture drives the real controller with API status subresources.
type runtimeRecoveryFixture struct {
	ctx        context.Context
	client     client.WithWatch
	reconciler *WorkspaceReconciler
	workspace  *workspacesv1alpha1.Workspace
	pod        *corev1.Pod
}

func newRuntimeRecoveryFixture(t *testing.T) *runtimeRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, rbacv1.AddToScheme(scheme))
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "recovery", Namespace: testNamespace, UID: "workspace-uid"},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState: workspacesv1alpha1.WorkspaceDesiredStateRunning, Image: testRuntimeImage,
			Storage: &workspacesv1alpha1.PersistentStorageSpec{StorageClassName: testStorageClass, Size: resource.MustParse("1Gi")},
		},
	}
	home := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace, OwnerReferences: []metav1.OwnerReference{{UID: workspace.UID, Controller: boolPointer(true)}}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(workspace, &workspacesv1alpha1.WorkspaceExec{}, home, &corev1.Pod{}).
		WithObjects(workspace, home).Build()
	fixture := &runtimeRecoveryFixture{ctx: ctx, client: kubeClient, workspace: workspace}
	reconciler := &WorkspaceReconciler{Client: kubeClient, Scheme: scheme, RunnerImage: testRunnerImage}
	fixture.reconciler = reconciler
	key := client.ObjectKeyFromObject(workspace)
	request := reconcile.Request{NamespacedName: key}
	_, err := reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	pod := new(corev1.Pod)
	require.NoError(t, kubeClient.Get(ctx, key, pod))
	pod.UID = types.UID("original-runtime-uid")
	require.NoError(t, kubeClient.Update(ctx, pod))

	require.NoError(t, kubeClient.Get(ctx, key, workspace))
	fixture.pod = pod
	return fixture
}

// restart discards controller memory while preserving only API objects.
func (fixture *runtimeRecoveryFixture) restart() {
	fixture.reconciler = &WorkspaceReconciler{Client: fixture.client, APIReader: fixture.client, Scheme: fixture.reconciler.Scheme, RunnerImage: testRunnerImage}
}

func (fixture *runtimeRecoveryFixture) reconcile(t *testing.T) reconcile.Result {
	t.Helper()
	result, err := fixture.reconciler.Reconcile(fixture.ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(fixture.workspace)})
	require.NoError(t, err)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), fixture.workspace))
	return result
}

func (fixture *runtimeRecoveryFixture) setPodReady(t *testing.T, uid string) *corev1.Pod {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), pod))
	pod.UID = types.UID(uid)
	require.NoError(t, fixture.client.Update(fixture.ctx, pod))
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	require.NoError(t, fixture.client.Status().Update(fixture.ctx, pod))
	fixture.pod = pod
	return pod
}

func TestRuntimeRecoveryWaitsForMountCleanupAndPodDeletion(t *testing.T) {
	const recoveryMountName = "recovery-code"
	fixture := newRuntimeRecoveryFixture(t)
	fixture.pod.Spec.NodeName = "old-node"
	fixture.pod.Finalizers = []string{"test/retain-until-unmounted"}
	require.NoError(t, fixture.client.Update(fixture.ctx, fixture.pod))
	fixture.pod.Status.Phase = corev1.PodFailed
	require.NoError(t, fixture.client.Status().Update(fixture.ctx, fixture.pod))
	claims := []workspaceWriteClaim{{leaseName: "old-write-lease", worktree: "old-worktree"}}
	acquired, _, err := fixture.reconciler.acquireWriteClaims(fixture.ctx, fixture.workspace, claims)
	require.NoError(t, err)
	require.True(t, acquired)
	helper, err := hotMountHelperPod(fixture.workspace, fixture.pod, hotWorktreeMount{name: recoveryMountName, worktree: "old-worktree", path: recoveryMountName, claimName: "old-pvc"}, testRunnerImage)
	require.NoError(t, err)
	helper.OwnerReferences = []metav1.OwnerReference{{UID: fixture.workspace.UID, Controller: boolPointer(true)}}
	helper.Status.Phase = corev1.PodFailed
	require.NoError(t, fixture.client.Create(fixture.ctx, helper))
	fixture.reconcile(t)
	cleaner := new(corev1.Pod)
	cleanerKey := client.ObjectKey{Namespace: helper.Namespace, Name: helper.Name + "-clean"}
	require.NoError(t, fixture.client.Get(fixture.ctx, cleanerKey, cleaner))
	require.Equal(t, "old-node", cleaner.Spec.NodeName)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.pod), fixture.pod), "keep old runtime while failed mount cleanup is incomplete")
	require.True(t, fixture.pod.DeletionTimestamp.IsZero())
	leaseKey := client.ObjectKey{Namespace: fixture.workspace.Namespace, Name: claims[0].leaseName}
	require.NoError(t, fixture.client.Get(fixture.ctx, leaseKey, new(coordinationv1.Lease)))
	cleaner.Status.Phase = corev1.PodSucceeded
	require.NoError(t, fixture.client.Status().Update(fixture.ctx, cleaner))
	// First remove the helper, then its cleanup Pod, then request runtime deletion.
	for range 3 {
		fixture.restart()
		fixture.reconcile(t)
	}
	require.True(t, apierrors.IsNotFound(fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(helper), new(corev1.Pod))))
	require.True(t, apierrors.IsNotFound(fixture.client.Get(fixture.ctx, cleanerKey, new(corev1.Pod))))
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.pod), fixture.pod))
	require.False(t, fixture.pod.DeletionTimestamp.IsZero())
	fixture.reconcile(t)
	require.NoError(t, fixture.client.Get(fixture.ctx, leaseKey, new(coordinationv1.Lease)), "terminating runtime still holds its write Lease")
	fixture.pod.Finalizers = nil
	require.NoError(t, fixture.client.Update(fixture.ctx, fixture.pod))
	fixture.restart()
	fixture.reconcile(t)
	require.True(t, apierrors.IsNotFound(fixture.client.Get(fixture.ctx, leaseKey, new(coordinationv1.Lease))), "only release after runtime and helper absence")
	replacement := new(corev1.Pod)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), replacement))
	require.NotEqual(t, fixture.pod.UID, replacement.UID)
}

func TestAbsentRuntimeKeepsLeaseUntilOrphanMountIsGone(t *testing.T) {
	fixture := newRuntimeRecoveryFixture(t)
	claims := []workspaceWriteClaim{{leaseName: "orphan-write-lease", worktree: "orphan-worktree"}}
	_, _, err := fixture.reconciler.acquireWriteClaims(fixture.ctx, fixture.workspace, claims)
	require.NoError(t, err)
	helper := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "orphan-helper", Namespace: fixture.workspace.Namespace,
		Labels:          map[string]string{workspaceManagedByLabel: fixture.workspace.Name, hotMountHelperLabel: hotMountLabelValue},
		OwnerReferences: []metav1.OwnerReference{{UID: fixture.workspace.UID, Controller: boolPointer(true)}},
		Finalizers:      []string{"test/hold-mount"},
	}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	require.NoError(t, fixture.client.Create(fixture.ctx, helper))
	require.NoError(t, fixture.client.Delete(fixture.ctx, fixture.pod))
	fixture.reconcile(t)
	leaseKey := client.ObjectKey{Namespace: fixture.workspace.Namespace, Name: claims[0].leaseName}
	require.NoError(t, fixture.client.Get(fixture.ctx, leaseKey, new(coordinationv1.Lease)))
	require.True(t, apierrors.IsNotFound(fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.pod), new(corev1.Pod))), "do not create a runtime with stale mounts")
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(helper), helper))
	helper.Finalizers = nil
	require.NoError(t, fixture.client.Update(fixture.ctx, helper))
	fixture.reconcile(t)
	require.True(t, apierrors.IsNotFound(fixture.client.Get(fixture.ctx, leaseKey, new(coordinationv1.Lease))))
}

func TestTerminalRuntimeRecoveryPrecedesUnavailableDependencies(t *testing.T) {
	const missingMount = "missing"
	fixture := newRuntimeRecoveryFixture(t)
	fixture.pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, fixture.client.Status().Update(fixture.ctx, fixture.pod))
	fixture.workspace.Spec.Mounts = []workspacesv1alpha1.WorkspaceMount{{Name: missingMount, Path: missingMount, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: "missing-worktree"}}}
	require.NoError(t, fixture.client.Update(fixture.ctx, fixture.workspace))
	fixture.reconcile(t)
	require.Equal(t, "RuntimeCompleted", meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady).Reason)
	require.True(t, apierrors.IsNotFound(fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.pod), new(corev1.Pod))))
	fixture.reconcile(t)
	require.Equal(t, "WorktreeNotFound", meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady).Reason)
}

// podListLagClient simulates an informer that has not observed helper Pods yet.
type podListLagClient struct{ client.Client }

func (cached *podListLagClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if pods, ok := list.(*corev1.PodList); ok {
		pods.Items = nil
		return nil
	}
	return cached.Client.List(ctx, list, opts...)
}

func TestRuntimeReplacementReadsMountAbsenceFromAPI(t *testing.T) {
	fixture := newRuntimeRecoveryFixture(t)
	helper := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "lagging-helper", Namespace: fixture.workspace.Namespace,
		Labels:          map[string]string{workspaceManagedByLabel: fixture.workspace.Name, hotMountHelperLabel: hotMountLabelValue},
		OwnerReferences: []metav1.OwnerReference{{UID: fixture.workspace.UID, Controller: boolPointer(true)}},
		Finalizers:      []string{"test/mounted"},
	}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	require.NoError(t, fixture.client.Create(fixture.ctx, helper))
	require.NoError(t, fixture.client.Delete(fixture.ctx, fixture.pod))
	fixture.reconciler.APIReader = fixture.client
	fixture.reconciler.Client = &podListLagClient{Client: fixture.client}
	fixture.reconcile(t)
	require.True(t, apierrors.IsNotFound(fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.pod), new(corev1.Pod))), "cache absence cannot admit a runtime over an old mount")
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(helper), helper))
	require.False(t, helper.DeletionTimestamp.IsZero(), "cleanup uses the API reader's helper observation")
}

func TestQueuedExecutionDoesNotBlockTerminalRuntimeReplacement(t *testing.T) {
	fixture := newRuntimeRecoveryFixture(t)
	queued := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "unbound", Namespace: fixture.workspace.Namespace},
		Spec:       workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: fixture.workspace.Name}},
		Status:     workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhasePending},
	}
	require.NoError(t, fixture.client.Create(fixture.ctx, queued))
	fixture.pod.Status.Phase = corev1.PodFailed
	require.NoError(t, fixture.client.Status().Update(fixture.ctx, fixture.pod))
	fixture.reconcile(t)
	require.True(t, apierrors.IsNotFound(fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.pod), new(corev1.Pod))))
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(queued), queued))
	require.Equal(t, workspacesv1alpha1.WorkspaceExecPhasePending, queued.Status.Phase)
}

// ROOT CAUSE:
// A controller can stop between recording a terminal exit and deleting its Pod.
// Re-observing API objects must finish recovery without a persistent retry policy.
func TestTerminalRecoveryResumesAfterFailedWrite(t *testing.T) {
	for _, stage := range []string{"status", "delete"} {
		t.Run(stage, func(t *testing.T) {
			fixture := newRuntimeRecoveryFixture(t)
			fixture.pod.Status.Phase = corev1.PodFailed
			require.NoError(t, fixture.client.Status().Update(fixture.ctx, fixture.pod))
			interrupted := errors.New("controller interrupted")
			faults := interceptor.Funcs{}
			if stage == "status" {
				faults.SubResourceUpdate = func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					return interrupted
				}
			} else {
				faults.Delete = func(ctx context.Context, c client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
					current := new(workspacesv1alpha1.Workspace)
					require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(fixture.workspace), current))
					require.Equal(t, "RuntimeFailed", meta.FindStatusCondition(current.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady).Reason, "persist exit before requesting deletion")
					return interrupted
				}
			}
			fixture.reconciler.Client = interceptor.NewClient(fixture.client, faults)
			_, err := fixture.reconciler.Reconcile(fixture.ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(fixture.workspace)})
			require.ErrorIs(t, err, interrupted)
			require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.pod), new(corev1.Pod)), "failed write cannot bypass ordering")

			fixture.restart()
			fixture.reconcile(t)
			require.True(t, apierrors.IsNotFound(fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.pod), new(corev1.Pod))))
			fixture.restart()
			fixture.reconcile(t)
			fixture.setPodReady(t, "recovered-runtime")
			fixture.reconcile(t)
			require.True(t, meta.IsStatusConditionTrue(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady))
			require.True(t, meta.IsStatusConditionFalse(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionDegraded))
		})
	}
}

func TestTerminalRecoveryDeleteFencesReplacementUID(t *testing.T) {
	fixture := newRuntimeRecoveryFixture(t)
	fixture.pod.Status.Phase = corev1.PodFailed
	require.NoError(t, fixture.client.Status().Update(fixture.ctx, fixture.pod))
	fixture.reconciler.Client = interceptor.NewClient(fixture.client, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			options := (&client.DeleteOptions{}).ApplyOptions(opts)
			require.NotNil(t, options.Preconditions)
			require.NotNil(t, options.Preconditions.UID)
			require.Equal(t, fixture.pod.UID, *options.Preconditions.UID)
			require.NoError(t, c.Delete(ctx, obj))
			replacement := fixture.pod.DeepCopy()
			replacement.ResourceVersion, replacement.UID = "", "concurrent-replacement"
			replacement.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
			require.NoError(t, c.Create(ctx, replacement))
			// NOTICE:
			// Simulate API server UID fencing after replacement between read and delete.
			// The fake client only checks ResourceVersion preconditions.
			// Source: controller-runtime v0.24.1, pkg/client/fake/client.go:680-727.
			// Remove this emulation when fake Delete enforces UID preconditions.
			return apierrors.NewConflict(corev1.Resource("pods"), obj.GetName(), errors.New("UID precondition failed"))
		},
	})
	_, err := fixture.reconciler.Reconcile(fixture.ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(fixture.workspace)})
	require.True(t, apierrors.IsConflict(err))
	fixture.restart()
	fixture.reconcile(t)
	replacement := new(corev1.Pod)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.pod), replacement))
	require.Equal(t, types.UID("concurrent-replacement"), replacement.UID)
	require.True(t, meta.IsStatusConditionTrue(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady))
}

func TestWorkspaceExecDoesNotStartWhenClaimedRuntimeTerminates(t *testing.T) {
	fixture := newRuntimeRecoveryFixture(t)
	fixture.setPodReady(t, "claimed-runtime")
	fixture.reconcile(t)
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-race", Namespace: fixture.workspace.Namespace, UID: "exec-uid", Finalizers: []string{executionFinalizer}},
		Spec: workspacesv1alpha1.WorkspaceExecSpec{
			TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: fixture.workspace.Name},
			Command:   []string{"cat"},
		},
	}
	require.NoError(t, fixture.client.Create(fixture.ctx, process))
	claimClient := interceptor.NewClient(fixture.client, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := c.SubResource(subresource).Update(ctx, obj, opts...); err != nil {
				return err
			}
			if execution, ok := obj.(*workspacesv1alpha1.WorkspaceExec); ok && execution.Status.Phase == workspacesv1alpha1.WorkspaceExecPhaseStarting {
				currentPod := new(corev1.Pod)
				if err := c.Get(ctx, client.ObjectKeyFromObject(fixture.pod), currentPod); err != nil {
					return err
				}
				currentPod.Status.Phase = corev1.PodSucceeded
				return c.Status().Update(ctx, currentPod)
			}
			return nil
		},
	})
	processRuntime := &recordingProcessRuntime{}
	reconciler := &WorkspaceExecReconciler{Client: claimClient, APIReader: fixture.client, Scheme: fixture.reconciler.Scheme, Runtime: processRuntime}
	_, err := reconciler.Reconcile(fixture.ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)})
	require.NoError(t, err)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(process), process))
	require.Equal(t, workspacesv1alpha1.WorkspaceExecPhaseLost, process.Status.Phase)
	require.Empty(t, processRuntime.startedRequest.ID, "detect terminal UID after claiming and before starting")
}
