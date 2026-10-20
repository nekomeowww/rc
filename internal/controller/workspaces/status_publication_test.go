package workspaces

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// countStatusWrites wraps base and counts status subresource writes.
func countStatusWrites(base client.WithWatch, writes *int) client.WithWatch {
	return interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			*writes++
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			*writes++
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
}

func TestLifecycleDeadlines(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	created := now.Add(-2 * time.Hour)
	at := func(t time.Time) *time.Time { return &t }
	suspended := func(ws *workspacesv1alpha1.Workspace, reason string) {
		ws.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
		ws.Status.SuspendedAt = &metav1.Time{Time: now.Add(-30 * time.Minute)}
		ws.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionFalse, Reason: reason}}
	}
	completion := metav1.NewTime(now.Add(-time.Minute))
	for _, tc := range []struct {
		name      string
		configure func(*workspacesv1alpha1.Workspace)
		processes workspaceProcesses
		want      workspaceDeadlines
	}{
		{name: "idle timeout", want: workspaceDeadlines{idleSuspendAt: at(now.Add(-10*time.Minute + time.Hour))}},
		{name: "idle disabled", configure: func(ws *workspacesv1alpha1.Workspace) { ws.Spec.IdleTimeout = &metav1.Duration{} }},
		{name: "active blocks idle", processes: workspaceProcesses{active: 1, any: true}},
		{name: "suspended confirmed", configure: func(ws *workspacesv1alpha1.Workspace) { suspended(ws, reasonSuspended) }, want: workspaceDeadlines{deleteAt: at(now.Add(30 * time.Minute))}},
		{name: "suspended still stopping", configure: func(ws *workspacesv1alpha1.Workspace) { suspended(ws, "Stopping") }},
		{name: "suspended without opt-in", configure: func(ws *workspacesv1alpha1.Workspace) {
			suspended(ws, reasonSuspended)
			ws.Spec.DeleteAfterSuspended = nil
		}},
		{name: "active blocks suspended deletion", configure: func(ws *workspacesv1alpha1.Workspace) { suspended(ws, reasonSuspended) }, processes: workspaceProcesses{active: 2, any: true}},
		{name: "temporary never started", configure: func(ws *workspacesv1alpha1.Workspace) {
			ws.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
		}, want: workspaceDeadlines{deleteAt: at(created.Add(temporaryWorkspaceStartTimeout))}},
		{name: "temporary without completion time", configure: func(ws *workspacesv1alpha1.Workspace) {
			ws.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
		}, processes: workspaceProcesses{any: true}, want: workspaceDeadlines{recheck: temporaryWorkspaceCleanupDelay}},
		{name: "temporary completed", configure: func(ws *workspacesv1alpha1.Workspace) {
			ws.Spec.RetentionPolicy = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
		}, processes: workspaceProcesses{any: true, lastCompletion: &completion}, want: workspaceDeadlines{deleteAt: at(completion.Add(temporaryWorkspaceCleanupDelay))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ws := lifecycleWorkspace(now)
			ws.CreationTimestamp = metav1.NewTime(created)
			ws.Status.LastActivityTime = &metav1.Time{Time: now.Add(-10 * time.Minute)}
			if tc.configure != nil {
				tc.configure(ws)
			}
			assert.Equal(t, tc.want, lifecycleDeadlines(ws, tc.processes))
		})
	}
}

func TestWorkspaceDeadlinesRequeueAndPublishedPrecision(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	idle, deletion := now.Add(90*time.Second+300*time.Millisecond), now.Add(time.Hour)
	deadlines := workspaceDeadlines{idleSuspendAt: &idle, deleteAt: &deletion}
	assert.Equal(t, 90*time.Second+300*time.Millisecond, deadlines.requeueAfter(now), "act at the precise earliest deadline")
	assert.Equal(t, temporaryWorkspaceCleanupDelay, workspaceDeadlines{recheck: temporaryWorkspaceCleanupDelay}.requeueAfter(now))
	assert.Zero(t, workspaceDeadlines{}.requeueAfter(now))
	status := deadlines.status(3)
	assert.Equal(t, now.Add(91*time.Second), status.IdleSuspendAt.Time, "never publish a deadline before the action")
	assert.Equal(t, deletion, status.DeleteAt.Time)
	assert.Equal(t, int32(3), status.ActiveExecutions)
}

func TestRetentionPublishesLifecycleOnlyOnChange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	ready := now.Add(-10 * time.Minute)
	workspace := lifecycleWorkspace(now)
	workspace.Spec.IdleTimeout = &metav1.Duration{Duration: time.Hour + 500*time.Millisecond}
	workspace.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(ready)}}
	base := lifecycleClient(t, workspace).(client.WithWatch)
	persisted, delay := reconcileLifecycle(t, base, now)
	require.NotNil(t, persisted.Status.Lifecycle)
	assert.Equal(t, 50*time.Minute+500*time.Millisecond, delay, "RequeueAfter and status share one deadline")
	assert.Equal(t, ready.Add(time.Hour+time.Second), persisted.Status.Lifecycle.IdleSuspendAt.UTC())
	assert.Nil(t, persisted.Status.Lifecycle.DeleteAt)
	assert.Zero(t, persisted.Status.Lifecycle.ActiveExecutions)

	writes := 0
	_, _ = reconcileLifecycle(t, countStatusWrites(base, &writes), now.Add(time.Minute))
	assert.Zero(t, writes, "unchanged lifecycle and history must not patch status")

	require.NoError(t, base.Create(context.Background(), lifecycleExec(workspace, workspacesv1alpha1.WorkspaceExecPhaseRunning)))
	persisted, delay = reconcileLifecycle(t, base, now.Add(2*time.Minute))
	assert.Equal(t, int32(1), persisted.Status.Lifecycle.ActiveExecutions)
	assert.Nil(t, persisted.Status.Lifecycle.IdleSuspendAt, "active executions block idle suspension")
	assert.Zero(t, delay)
}

func TestRetentionPublishesSuspendedDeleteDeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	workspace := lifecycleWorkspace(now)
	workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
	workspace.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionFalse, Reason: reasonSuspended}}
	persisted, delay := reconcileLifecycle(t, lifecycleClient(t, workspace), now)
	require.NotNil(t, persisted.Status.SuspendedAt, "stamp a legacy suspension in the same patch")
	assert.Equal(t, now.Add(time.Hour), persisted.Status.Lifecycle.DeleteAt.UTC())
	assert.Equal(t, time.Hour, delay)
}

func TestWorkspaceStorageReadyReportsBoundLostAndConflict(t *testing.T) {
	t.Parallel()
	fixture := newRuntimeRecoveryFixture(t)
	storage := meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionStorageReady)
	require.NotNil(t, storage)
	assert.Equal(t, metav1.ConditionTrue, storage.Status)
	assert.Equal(t, workspacesv1alpha1.ReasonVolumeClaimBound, storage.Reason)

	home := new(corev1.PersistentVolumeClaim)
	homeKey := client.ObjectKey{Namespace: fixture.workspace.Namespace, Name: fixture.workspace.Status.HomeVolumeClaimName}
	require.NoError(t, fixture.client.Get(fixture.ctx, homeKey, home))
	home.OwnerReferences[0].UID = "previous-workspace"
	require.NoError(t, fixture.client.Update(fixture.ctx, home))
	fixture.reconcile(t)
	storage = meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionStorageReady)
	assert.Equal(t, metav1.ConditionFalse, storage.Status)
	assert.Equal(t, workspacesv1alpha1.ReasonVolumeClaimConflict, storage.Reason)

	require.NoError(t, fixture.client.Delete(fixture.ctx, home))
	fixture.reconcile(t)
	storage = meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionStorageReady)
	assert.Equal(t, metav1.ConditionFalse, storage.Status)
	assert.Equal(t, workspacesv1alpha1.ReasonVolumeClaimLost, storage.Reason)
	assert.Equal(t, workspacesv1alpha1.ReasonVolumeClaimLost, meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady).Reason)
	assert.True(t, apierrors.IsNotFound(fixture.client.Get(fixture.ctx, homeKey, new(corev1.PersistentVolumeClaim))), "never recreate a lost home")
}

func TestWorkspaceReportsRuntimeMissingBeforeReplacement(t *testing.T) {
	t.Parallel()
	fixture := newRuntimeRecoveryFixture(t)
	fixture.setPodReady(t, "ready-runtime")
	fixture.reconcile(t)
	require.True(t, meta.IsStatusConditionTrue(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady))
	require.NoError(t, fixture.client.Delete(fixture.ctx, fixture.pod))
	fixture.reconcile(t)
	ready := meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonRuntimeMissing, ready.Reason)
	assert.Contains(t, ready.Message, fixture.pod.Name)
	fixture.reconcile(t)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), new(corev1.Pod)), "replace the missing runtime")
	assert.Equal(t, reasonStarting, meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady).Reason)
}

func TestWorkspaceDeletionBlockedNamesBlockers(t *testing.T) {
	t.Parallel()
	const foreignFinalizer = "example.com/keep"
	fixture := newRuntimeRecoveryFixture(t)
	execution := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "still-running", Namespace: fixture.workspace.Namespace},
		Spec:       workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: fixture.workspace.Name}, Command: []string{testTrueValue}},
		Status:     workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseRunning},
	}
	require.NoError(t, fixture.client.Create(fixture.ctx, execution))
	fixture.workspace.Finalizers = append(fixture.workspace.Finalizers, foreignFinalizer)
	require.NoError(t, fixture.client.Update(fixture.ctx, fixture.workspace))
	require.NoError(t, fixture.client.Delete(fixture.ctx, fixture.workspace))

	fixture.reconcile(t)
	blocked := meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionDeletionBlocked)
	require.NotNil(t, blocked)
	assert.Equal(t, metav1.ConditionTrue, blocked.Status)
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonWaitingForExecutions, blocked.Reason)
	assert.Contains(t, blocked.Message, execution.Name)

	writes := 0
	fixture.reconciler.Client = countStatusWrites(fixture.client, &writes)
	fixture.reconcile(t)
	assert.Zero(t, writes, "no patch while the blocker is unchanged")

	require.NoError(t, fixture.client.Delete(fixture.ctx, execution))
	fixture.reconcile(t)
	blocked = meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionDeletionBlocked)
	require.NotNil(t, blocked)
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonWaitingForPods, blocked.Reason)
	assert.Equal(t, 1, writes, "a changed blocker is one status patch")
	assert.Contains(t, blocked.Message, fixture.pod.Name)

	fixture.reconcile(t)
	assert.Nil(t, meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionDeletionBlocked), "clear the blocker when the finalizer is done")
	assert.Equal(t, []string{foreignFinalizer}, fixture.workspace.Finalizers)
}

func TestEnvironmentStorageReadyReportsBoundAndLost(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	environment := &workspacesv1alpha1.WorkspaceEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-env", Namespace: testNamespace, UID: "storage-env-uid"},
		Spec:       workspacesv1alpha1.WorkspaceEnvironmentSpec{Image: testRuntimeImage, Storage: workspacesv1alpha1.PersistentStorageSpec{StorageClassName: testStorageClass, Size: resource.MustParse("1Gi")}},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(environment, &corev1.PersistentVolumeClaim{}).WithObjects(environment).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).Build()
	reconciler := &WorkspaceEnvironmentReconciler{Client: kube, Scheme: scheme}
	key := client.ObjectKeyFromObject(environment)
	reconcileEnvironment := func() *metav1.Condition {
		t.Helper()
		_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: key})
		require.NoError(t, err)
		require.NoError(t, kube.Get(t.Context(), key, environment))
		return meta.FindStatusCondition(environment.Status.Conditions, workspacesv1alpha1.WorkspaceEnvironmentConditionStorageReady)
	}
	storage := reconcileEnvironment()
	assert.Equal(t, "Provisioning", storage.Reason)
	claim := new(corev1.PersistentVolumeClaim)
	claimKey := client.ObjectKey{Namespace: testNamespace, Name: environment.Status.CurrentVolumeClaimName}
	require.NoError(t, kube.Get(t.Context(), claimKey, claim))
	claim.Status.Phase = corev1.ClaimBound
	require.NoError(t, kube.Status().Update(t.Context(), claim))
	storage = reconcileEnvironment()
	assert.Equal(t, metav1.ConditionTrue, storage.Status)
	assert.Equal(t, workspacesv1alpha1.ReasonVolumeClaimBound, storage.Reason)

	require.NoError(t, kube.Delete(t.Context(), claim))
	storage = reconcileEnvironment()
	assert.Equal(t, metav1.ConditionFalse, storage.Status)
	assert.Equal(t, workspacesv1alpha1.ReasonVolumeClaimLost, storage.Reason)
	assert.True(t, apierrors.IsNotFound(kube.Get(t.Context(), claimKey, new(corev1.PersistentVolumeClaim))), "never recreate committed content")
}
