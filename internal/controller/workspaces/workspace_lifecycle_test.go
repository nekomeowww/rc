package workspaces

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
)

const lifecycleExecutionName = "execution"

func lifecycleWorkspace(now time.Time) *workspacesv1alpha1.Workspace {
	return &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "lifecycle", Namespace: testNamespace, CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Hour)), Finalizers: []string{workspaceFinalizer}},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState:         workspacesv1alpha1.WorkspaceDesiredStateRunning,
			IdleTimeout:          &metav1.Duration{Duration: time.Hour},
			DeleteAfterSuspended: &metav1.Duration{Duration: time.Hour},
		},
	}
}

func lifecycleClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&workspacesv1alpha1.Workspace{}, &workspacesv1alpha1.WorkspaceExec{}).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).WithObjects(objects...).Build()
}

func lifecycleExec(workspace *workspacesv1alpha1.Workspace, phase workspacesv1alpha1.WorkspaceExecPhase) *workspacesv1alpha1.WorkspaceExec {
	return &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: lifecycleExecutionName, Namespace: workspace.Namespace},
		Spec:       workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name}, Command: []string{"true"}},
		Status:     workspacesv1alpha1.WorkspaceExecStatus{Phase: phase},
	}
}

func reconcileLifecycle(t *testing.T, kubeClient client.Client, now time.Time) (*workspacesv1alpha1.Workspace, time.Duration) {
	t.Helper()
	key := client.ObjectKey{Namespace: testNamespace, Name: "lifecycle"}
	// Construct a fresh reconciler each time: all deadlines must survive restart.
	result, err := (&WorkspaceRetentionReconciler{Client: kubeClient, Now: func() time.Time { return now }}).Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, kubeClient.Get(context.Background(), key, persisted))
	return persisted, result.RequeueAfter
}

func TestLifecycleIdleOrigins(t *testing.T) {
	t.Parallel()
	const missingEnvironment = "missing"
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	t.Run("never executed or ready", func(t *testing.T) {
		workspace := lifecycleWorkspace(now)
		// ROOT CAUSE: hasProcesses and topology resolution previously excluded
		// zero-execution and unhealthy Workspaces from the idle decision entirely.
		workspace.Spec.EnvironmentRef = &workspacesv1alpha1.LocalReference{Name: missingEnvironment}
		persisted, _ := reconcileLifecycle(t, lifecycleClient(t, workspace), now)
		assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateSuspended, persisted.Spec.DesiredState)
	})
	t.Run("fresh Ready grants usable time", func(t *testing.T) {
		workspace := lifecycleWorkspace(now)
		workspace.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Minute))}}
		persisted, delay := reconcileLifecycle(t, lifecycleClient(t, workspace), now)
		assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateRunning, persisted.Spec.DesiredState)
		assert.Equal(t, 50*time.Minute, delay)
	})
	t.Run("completion survives history removal and restart", func(t *testing.T) {
		workspace := lifecycleWorkspace(now)
		process := lifecycleExec(workspace, workspacesv1alpha1.WorkspaceExecPhaseSucceeded)
		process.Status.CompletedAt = &metav1.Time{Time: now.Add(-10 * time.Minute)}
		kubeClient := lifecycleClient(t, workspace, process)
		_, delay := reconcileLifecycle(t, kubeClient, now)
		assert.Equal(t, 50*time.Minute, delay)
		require.NoError(t, kubeClient.Delete(context.Background(), process))
		persisted, delay := reconcileLifecycle(t, kubeClient, now.Add(20*time.Minute))
		assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateRunning, persisted.Spec.DesiredState)
		assert.Equal(t, 30*time.Minute, delay)
		persisted, _ = reconcileLifecycle(t, kubeClient, now.Add(50*time.Minute))
		assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateSuspended, persisted.Spec.DesiredState)
	})
	t.Run("disabled policy keeps retained machine", func(t *testing.T) {
		workspace := lifecycleWorkspace(now)
		workspace.Spec.IdleTimeout = nil
		workspace.Spec.DeleteAfterSuspended = nil
		persisted, delay := reconcileLifecycle(t, lifecycleClient(t, workspace), now)
		assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateRunning, persisted.Spec.DesiredState)
		assert.Zero(t, delay)
	})
}

func TestLifecycleActiveExecutionsBlockEveryStage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, phase := range []workspacesv1alpha1.WorkspaceExecPhase{"", workspacesv1alpha1.WorkspaceExecPhasePending, workspacesv1alpha1.WorkspaceExecPhaseRunning} {
		t.Run(string(phase), func(t *testing.T) {
			for _, stage := range []string{"suspend compute", "delete suspended", "delete temporary"} {
				t.Run(stage, func(t *testing.T) {
					workspace := lifecycleWorkspace(now)
					if stage == "delete suspended" {
						workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
						workspace.Status.SuspendedAt = &metav1.Time{Time: now.Add(-2 * time.Hour)}
						workspace.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionFalse, Reason: reasonSuspended}}
					}
					if stage == "delete temporary" {
						workspace.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
					}
					persisted, _ := reconcileLifecycle(t, lifecycleClient(t, workspace, lifecycleExec(workspace, phase)), now)
					assert.True(t, persisted.DeletionTimestamp.IsZero())
					assert.Equal(t, workspace.Spec.DesiredState, persisted.Spec.DesiredState)
				})
			}
		})
	}
}

func TestLifecycleSuspendedRecoveryWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	workspace := lifecycleWorkspace(now)
	workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
	workspace.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionFalse, Reason: reasonSuspended, LastTransitionTime: metav1.NewTime(now.Add(-24 * time.Hour))}}
	kubeClient := lifecycleClient(t, workspace)
	// A legacy Ready=False timestamp is not the suspension time. Grant a new
	// full grace window once, then retain it through controller reconstruction.
	persisted, delay := reconcileLifecycle(t, kubeClient, now)
	assert.Equal(t, time.Hour, delay)
	assert.True(t, persisted.DeletionTimestamp.IsZero())
	assert.True(t, persisted.Status.SuspendedAt.Time.Equal(now))
	persisted, delay = reconcileLifecycle(t, kubeClient, now.Add(30*time.Minute))
	assert.Equal(t, 30*time.Minute, delay)
	assert.True(t, persisted.DeletionTimestamp.IsZero())
	persisted, _ = reconcileLifecycle(t, kubeClient, now.Add(time.Hour))
	assert.False(t, persisted.DeletionTimestamp.IsZero())
}

func TestLifecycleSuspendedDeletionRequiresOptInAndConfirmation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	const resumedScenario = "resumed"
	for _, scenario := range []string{"missing policy", "disabled", "stopping", "stale generation", resumedScenario} {
		t.Run(scenario, func(t *testing.T) {
			workspace := lifecycleWorkspace(now)
			workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
			workspace.Status.SuspendedAt = &metav1.Time{Time: now.Add(-2 * time.Hour)}
			workspace.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionFalse, Reason: reasonSuspended}}
			switch scenario {
			case "missing policy":
				workspace.Spec.IdleTimeout = nil
				workspace.Spec.DeleteAfterSuspended = nil
			case "disabled":
				workspace.Spec.DeleteAfterSuspended.Duration = 0
			case "stopping":
				workspace.Status.Conditions[0].Reason = "Stopping"
			case "stale generation":
				workspace.Generation = 2
			case resumedScenario:
				workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateRunning
			}
			persisted, _ := reconcileLifecycle(t, lifecycleClient(t, workspace), now)
			assert.True(t, persisted.DeletionTimestamp.IsZero())
			if scenario == resumedScenario {
				assert.Nil(t, persisted.Status.SuspendedAt)
				assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateRunning, persisted.Spec.DesiredState)
			}
		})
	}
}

func TestLifecycleUsesAuthoritativeExecutionState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	workspace := lifecycleWorkspace(now)
	workspace.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
	cache := lifecycleClient(t, workspace)
	authoritative := lifecycleClient(t, workspace, lifecycleExec(workspace, workspacesv1alpha1.WorkspaceExecPhaseRunning))
	_, err := (&WorkspaceRetentionReconciler{Client: cache, APIReader: authoritative, Now: func() time.Time { return now }}).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	require.NoError(t, err)
	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, cache.Get(context.Background(), client.ObjectKeyFromObject(workspace), persisted))
	assert.True(t, persisted.DeletionTimestamp.IsZero(), "cache may not yet contain the running execution")
}

func TestConfirmedSuspensionHasItsOwnClock(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	workspace := lifecycleWorkspace(now)
	workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
	workspace.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionFalse, Reason: "Stopping", LastTransitionTime: metav1.NewTime(now.Add(-24 * time.Hour))}}
	kubeClient := lifecycleClient(t, workspace)
	reconciler := &WorkspaceReconciler{Client: kubeClient}
	key := client.ObjectKeyFromObject(workspace)
	require.NoError(t, reconciler.setWorkspaceStatus(context.Background(), key, nil, metav1.ConditionFalse, reasonSuspended, "stopped"))
	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, kubeClient.Get(context.Background(), key, persisted))
	require.NotNil(t, persisted.Status.SuspendedAt)
	assert.False(t, persisted.Status.SuspendedAt.Before(&metav1.Time{Time: now}))
	assert.True(t, workspace.Status.Conditions[0].LastTransitionTime.Equal(&persisted.Status.Conditions[0].LastTransitionTime), "Ready=False does not transition when its reason changes")
	firstSuspension := persisted.Status.SuspendedAt.DeepCopy()
	require.NoError(t, reconciler.setWorkspaceStatus(context.Background(), key, nil, metav1.ConditionFalse, reasonSuspended, "stopped"))
	require.NoError(t, kubeClient.Get(context.Background(), key, persisted))
	assert.Equal(t, firstSuspension, persisted.Status.SuspendedAt, "reconciliation does not restart the clock")
}

func TestLifecycleRechecksExecutionsAtDeletionBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	workspace := lifecycleWorkspace(now)
	workspace.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	lists := 0
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).WithObjects(workspace).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, kubeClient client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists++
			if lists == 2 {
				require.NoError(t, kubeClient.Create(ctx, lifecycleExec(workspace, workspacesv1alpha1.WorkspaceExecPhasePending)))
			}
			return kubeClient.List(ctx, list, opts...)
		},
	}).Build()
	persisted, _ := reconcileLifecycle(t, kubeClient, now)
	assert.True(t, persisted.DeletionTimestamp.IsZero(), "execution arrived while activity status was being persisted")
	assert.Equal(t, 3, lists, "lifecycle boundary plus transcript obligation scan")
}

func TestLifecycleDeletePreconditionProtectsConcurrentPolicyEdit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	workspace := lifecycleWorkspace(now)
	workspace.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).WithObjects(workspace).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, kubeClient client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
			current := new(workspacesv1alpha1.Workspace)
			require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(object), current))
			current.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyRetain
			require.NoError(t, kubeClient.Update(ctx, current))
			return kubeClient.Delete(ctx, object, opts...)
		},
	}).Build()
	_, err := (&WorkspaceRetentionReconciler{Client: kubeClient, Now: func() time.Time { return now }}).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	require.True(t, apierrors.IsConflict(err), "do not delete with a stale resource version: %v", err)
	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKeyFromObject(workspace), persisted))
	assert.True(t, persisted.DeletionTimestamp.IsZero())
}

func TestTemporaryTerminalWithoutCompletionTimeWaits(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	workspace := lifecycleWorkspace(now)
	workspace.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
	persisted, delay := reconcileLifecycle(t, lifecycleClient(t, workspace, lifecycleExec(workspace, workspacesv1alpha1.WorkspaceExecPhaseSucceeded)), now)
	assert.True(t, persisted.DeletionTimestamp.IsZero())
	assert.Equal(t, executionRetentionInterval, delay)
}

// ROOT CAUSE: a child CREATE does not change the Workspace resourceVersion.
// A live LIST followed by preconditioned DELETE therefore still races a new
// execution; the runtime controller must participate in the deletion fence.
func TestAutomaticDeletionFencesDirectAPICreationAtDelete(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	workspace := lifecycleWorkspace(now)
	workspace.UID = "owner-uid"
	workspace.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
	workspace.Status.RuntimePodName = workspace.Name
	workspace.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace, UID: "runtime-uid"}}
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	runtimeClient := &recordingProcessRuntime{startState: processruntime.State{Phase: string(workspacesv1alpha1.WorkspaceExecPhaseRunning)}}
	intercepted := false
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace, &workspacesv1alpha1.WorkspaceExec{}).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).WithObjects(workspace, pod).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, kubeClient client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
			intercepted = true
			process := lifecycleExec(workspace, "")
			process.UID = "late-exec-uid"
			require.NoError(t, kubeClient.Create(ctx, process), "direct API create after the last active list")
			reconciler := &WorkspaceExecReconciler{Client: kubeClient, Scheme: scheme, Runtime: runtimeClient}
			request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)}
			_, err := reconciler.Reconcile(ctx, request)
			require.NoError(t, err)
			_, err = reconciler.Reconcile(ctx, request)
			require.NoError(t, err)
			return kubeClient.Delete(ctx, object, opts...)
		},
	}).Build()
	persisted, _ := reconcileLifecycle(t, kubeClient, now)
	require.True(t, intercepted)
	late := new(workspacesv1alpha1.WorkspaceExec)
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: lifecycleExecutionName, Namespace: workspace.Namespace}, late))
	assert.Equal(t, workspacesv1alpha1.WorkspaceExecPhaseFailed, late.Status.Phase)
	assert.Equal(t, "WorkspaceAdmissionRejected", late.Status.TerminationReason)
	assert.Empty(t, late.OwnerReferences, "unadmitted submissions do not acquire Workspace ownership")
	assert.True(t, persisted.DeletionTimestamp.IsZero() || runtimeClient.startedRequest.ID == "", "automatic deletion must not interrupt a command admitted after the last list")
}

func TestAutomaticDeletionLosesToExecutionAdmissionBeforeClosure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	workspace := lifecycleWorkspace(now)
	workspace.UID = "owner-uid"
	workspace.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
	workspace.Status.RuntimePodName = workspace.Name
	workspace.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace, UID: "runtime-uid"}}
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	runtimeClient := &recordingProcessRuntime{startState: processruntime.State{Phase: string(workspacesv1alpha1.WorkspaceExecPhaseRunning)}}
	intercepted := false
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace, &workspacesv1alpha1.WorkspaceExec{}).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).WithObjects(workspace, pod).WithInterceptorFuncs(interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, kubeClient client.Client, subResource string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			candidate, ok := object.(*workspacesv1alpha1.Workspace)
			if ok && candidate.Status.ExecutionAdmissionClosed && !intercepted {
				intercepted = true
				process := lifecycleExec(workspace, "")
				process.UID = "winning-execution-uid"
				require.NoError(t, kubeClient.Create(ctx, process))
				reconciler := &WorkspaceExecReconciler{Client: kubeClient, Scheme: scheme, Runtime: runtimeClient}
				request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)}
				_, err := reconciler.Reconcile(ctx, request)
				require.NoError(t, err)
				_, err = reconciler.Reconcile(ctx, request)
				require.NoError(t, err)
			}
			return kubeClient.SubResource(subResource).Patch(ctx, object, patch, opts...)
		},
	}).Build()
	_, err := (&WorkspaceRetentionReconciler{Client: kubeClient, Now: func() time.Time { return now }}).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	require.True(t, err == nil || apierrors.IsConflict(err), "%v", err)
	require.True(t, intercepted)
	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKeyFromObject(workspace), persisted))
	assert.True(t, persisted.DeletionTimestamp.IsZero())
	assert.False(t, persisted.Status.ExecutionAdmissionClosed)
	assert.Equal(t, lifecycleExecutionName, runtimeClient.startedRequest.ID)
	process := new(workspacesv1alpha1.WorkspaceExec)
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: lifecycleExecutionName, Namespace: workspace.Namespace}, process))
	assert.True(t, metav1.IsControlledBy(process, workspace), "controller establishes ownership only after successful admission")
	// A restart must still respect the admitted active execution.
	persisted, _ = reconcileLifecycle(t, kubeClient, now)
	assert.True(t, persisted.DeletionTimestamp.IsZero())
}

func TestRetentionRecoversFenceAfterRestartOrDeleteError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	t.Run("policy cancelled after crash", func(t *testing.T) {
		workspace := lifecycleWorkspace(now)
		workspace.Status.ExecutionAdmissionClosed = true
		workspace.Spec.IdleTimeout = nil
		workspace.Spec.DeleteAfterSuspended = nil
		persisted, _ := reconcileLifecycle(t, lifecycleClient(t, workspace), now)
		assert.False(t, persisted.Status.ExecutionAdmissionClosed)
		assert.True(t, persisted.DeletionTimestamp.IsZero())
	})
	t.Run("active submission after crash", func(t *testing.T) {
		workspace := lifecycleWorkspace(now)
		workspace.Status.ExecutionAdmissionClosed = true
		persisted, _ := reconcileLifecycle(t, lifecycleClient(t, workspace, lifecycleExec(workspace, workspacesv1alpha1.WorkspaceExecPhasePending)), now)
		assert.False(t, persisted.Status.ExecutionAdmissionClosed)
		assert.True(t, persisted.DeletionTimestamp.IsZero())
	})
	t.Run("expired fence resumes deletion", func(t *testing.T) {
		workspace := lifecycleWorkspace(now)
		workspace.Status.ExecutionAdmissionClosed = true
		workspace.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
		persisted, _ := reconcileLifecycle(t, lifecycleClient(t, workspace), now)
		assert.True(t, persisted.Status.ExecutionAdmissionClosed)
		assert.False(t, persisted.DeletionTimestamp.IsZero())
	})
	t.Run("delete error reopens admission", func(t *testing.T) {
		workspace := lifecycleWorkspace(now)
		workspace.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
		scheme := runtime.NewScheme()
		require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
		require.NoError(t, corev1.AddToScheme(scheme))
		kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).WithObjects(workspace).WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return fmt.Errorf("injected delete failure")
			},
		}).Build()
		_, err := (&WorkspaceRetentionReconciler{Client: kubeClient, Now: func() time.Time { return now }}).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
		require.ErrorContains(t, err, "injected delete failure")
		persisted := new(workspacesv1alpha1.Workspace)
		require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKeyFromObject(workspace), persisted))
		assert.False(t, persisted.Status.ExecutionAdmissionClosed)
		assert.True(t, persisted.DeletionTimestamp.IsZero())
	})
}
