package workspaces

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// A retained Workspace must not implicitly retain opted-in execution history forever.
func TestRetainedWorkspaceExecHistoryExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "retained", Namespace: testNamespace, UID: "retained-uid"}}
	// Decode the intended policy through the API seam so the pre-fix test compiles.
	require.NoError(t, json.Unmarshal([]byte(`{"retentionPolicy":"Retain","executionRetention":{"ttlAfterFinished":"24h","maxEntries":200}}`), &workspace.Spec))
	completed := metav1.NewTime(time.Now().Add(-72 * time.Hour))
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "old-history", Namespace: testNamespace, Finalizers: []string{executionFinalizer}},
		Spec:       workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name}, Command: []string{"true"}},
		Status:     workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, CompletedAt: &completed},
	}
	require.NoError(t, controllerutil.SetControllerReference(workspace, process, scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace, process).WithObjects(workspace, process).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).Build()
	r := &WorkspaceExecReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	key := client.ObjectKeyFromObject(process)
	retention := &WorkspaceRetentionReconciler{Client: kube, APIReader: kube}
	_, err := retention.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	require.NoError(t, err)
	for range 3 {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	persisted := new(workspacesv1alpha1.WorkspaceExec)
	err = kube.Get(ctx, key, persisted)
	require.True(t, err != nil || !persisted.DeletionTimestamp.IsZero(), "expired terminal execution must be collected while Workspace remains Retain")
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(workspace), new(workspacesv1alpha1.Workspace)))
}

func TestTerminalWorkspaceExecRetainsCleanupFinalizer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "terminal-with-transcript", Namespace: testNamespace, Finalizers: []string{executionFinalizer}},
		Status:     workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, TranscriptPath: ".rc/processes/terminal-with-transcript/transcript.log"},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(process).WithObjects(process).Build()
	r := &WorkspaceExecReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)})
	require.NoError(t, err)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(process), process))
	require.Contains(t, process.Finalizers, executionFinalizer, "direct CR deletion must retain a durable transcript cleanup obligation")
}

func TestRetentionBoundsOutstandingDeletions(t *testing.T) {
	t.Parallel()
	kube, workspace, original := retentionFixture(t)
	for i := range 30 {
		process := original.DeepCopy()
		process.Name = fmt.Sprintf("history-%02d", i)
		process.UID = types.UID(process.Name)
		process.ResourceVersion = ""
		require.NoError(t, kube.Create(t.Context(), process))
	}
	r := &executionRetentionService{Client: kube, APIReader: kube}
	for range 2 {
		_, err := r.reconcileExecutionHistory(t.Context(), workspace.Namespace, executionTargetReference(workspace), workspace.UID, workspace.Spec.ExecutionRetention)
		require.NoError(t, err)
	}
	list := new(workspacesv1alpha1.WorkspaceExecList)
	require.NoError(t, kube.List(t.Context(), list))
	deleting := 0
	for _, process := range list.Items {
		if !process.DeletionTimestamp.IsZero() {
			deleting++
			require.Contains(t, process.Finalizers, executionFinalizer)
		}
	}
	require.Equal(t, executionCleanupBatch, deleting)
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
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, process).WithStatusSubresource(workspace, process).Build()
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
