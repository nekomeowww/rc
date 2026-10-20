package workspaces

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/executionretention"
)

func TestTerminalWorkspaceExecExpiresAfterResultWindow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	completed := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "result-window", Namespace: testNamespace, UID: "result-window-uid"}}
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "result-window-exec", Namespace: testNamespace, UID: "result-window-exec-uid", Finalizers: []string{executionFinalizer}},
		Spec: workspacesv1alpha1.WorkspaceExecSpec{
			TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name},
			Command:   []string{"true"},
		},
		Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, CompletedAt: &metav1.Time{Time: completed}},
	}
	require.NoError(t, controllerutil.SetControllerReference(workspace, process, scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace, process).WithObjects(workspace, process).
		WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).Build()
	now := completed.Add(4 * time.Minute)
	r := &WorkspaceExecReconciler{Client: kube, APIReader: kube, Scheme: scheme, Now: func() time.Time { return now }}
	key := client.ObjectKeyFromObject(process)

	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	require.Equal(t, 6*time.Minute, result.RequeueAfter, "wait for the rest of the result window")
	require.NoError(t, kube.Get(ctx, key, process), "keep the record while its caller may read the result")

	now = completed.Add(executionResultWindow)
	for range 2 {
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	require.True(t, apierrors.IsNotFound(kube.Get(ctx, key, new(workspacesv1alpha1.WorkspaceExec))), "delete the record once the window ends")
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
	require.NotNil(t, workspace.Status.LastExecutionCompletedAt, "the finalizer keeps the idle clock")
}

type recordingHistoryPruner struct {
	calls    int
	policies []processruntime.HistoryPolicy
}

func (pruner *recordingHistoryPruner) PruneHistory(_ context.Context, _ processruntime.Target, policy processruntime.HistoryPolicy) error {
	pruner.calls++
	pruner.policies = append(pruner.policies, policy)
	return nil
}

func TestHistoryRetentionPrunesRunningSupervisorHourly(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: testNamespace, UID: "history-runtime-uid"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: testNamespace},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(running, pending).Build()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	pruner := &recordingHistoryPruner{}
	retention := newHistoryRetention(kube, pruner, func() time.Time { return now })
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws", Namespace: testNamespace, UID: "ws-uid"}}

	require.Equal(t, historyPruneInterval, retention.reconcile(t.Context(), workspace, running.Name, nil))
	require.Equal(t, 1, pruner.calls)
	require.Equal(t, executionretention.Policy(nil), pruner.policies[0], "retention is on by default")

	now = now.Add(20 * time.Minute)
	require.Equal(t, 40*time.Minute, retention.reconcile(t.Context(), workspace, running.Name, nil))
	require.Equal(t, 1, pruner.calls, "prune at most once per interval")

	now = now.Add(time.Hour)
	maxEntries := int32(10)
	retention.reconcile(t.Context(), workspace, running.Name, &workspacesv1alpha1.ExecutionRetentionPolicy{MaxEntries: maxEntries})
	require.Equal(t, 2, pruner.calls)
	require.Equal(t, 10, pruner.policies[1].MaxEntries, "send the current spec")

	replacement := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: testNamespace, UID: "replacement-uid"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	require.NoError(t, kube.Create(t.Context(), replacement))
	retention.reconcile(t.Context(), workspace, replacement.Name, nil)
	require.Equal(t, 3, pruner.calls, "prune a replaced runtime at once")

	stopped := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "stopped", Namespace: testNamespace, UID: "stopped-uid"}}
	require.Zero(t, retention.reconcile(t.Context(), stopped, "", nil), "a stopped machine has nothing to prune")
	require.Zero(t, retention.reconcile(t.Context(), stopped, pending.Name, nil))
	require.Equal(t, 3, pruner.calls)
}

// Execution history is not the idle clock: removing the last CR must not
// disable idle suspension for a retained Workspace.
func TestDeletingLastExecutionPreservesWorkspaceIdleClock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	completed := metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "idle-clock", Namespace: testNamespace, UID: "idle-clock-uid"}}
	process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: "last-result", Namespace: testNamespace, Finalizers: []string{executionFinalizer}}, Spec: workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name}}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, CompletedAt: &completed}}
	require.NoError(t, controllerutil.SetControllerReference(workspace, process, scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, process).WithStatusSubresource(workspace, process).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).Build()
	require.NoError(t, kube.Delete(ctx, process))
	_, err := (&WorkspaceExecReconciler{Client: kube, APIReader: kube, Scheme: scheme}).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)})
	require.NoError(t, err)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
	active, hadProcesses, last, err := workspaceProcessState(ctx, kube, workspace)
	require.NoError(t, err)
	require.False(t, active)
	require.True(t, hadProcesses, "history GC must not turn a previously active target into a never-used target")
	require.NotNil(t, last, "idle clock survives execution deletion")
	require.True(t, last.Equal(&completed))
}
