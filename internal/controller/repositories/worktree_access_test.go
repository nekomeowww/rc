package repositories

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	workspacecontroller "github.com/nekomeowww/rc/internal/controller/workspaces"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

func TestMountInsertedAfterFinalDeletionListIsNeverAdmitted(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, _ := ownershipStorage(t)
	now := metav1.Now()
	worktree.DeletionTimestamp = &now
	inserted := false
	kube := indexedFake(scheme).WithStatusSubresource(worktree, &workspacesv1alpha1.Workspace{}, &corev1.PersistentVolumeClaim{}).WithObjects(worktree).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			wt, ok := obj.(*repositoriesv1alpha1.Worktree)
			if ok && !controllerutil.ContainsFinalizer(wt, worktreeDeletionFinalizer) && !inserted {
				inserted = true
				token := false
				workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "late-consumer", Namespace: ownershipNamespace, UID: "late-consumer-uid"}, Spec: workspacesv1alpha1.WorkspaceSpec{Image: ownershipRunnerImage, Storage: &workspacesv1alpha1.PersistentStorageSpec{Size: resource.MustParse("1Gi")}, AutomountServiceAccountToken: &token, Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: ownershipWorktreeName, Path: ownershipWorktreeName, ReadOnly: true, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}}}}}
				require.NoError(t, c.Create(ctx, workspace))
				home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
				require.NoError(t, controllerutil.SetControllerReference(workspace, home, scheme))
				require.NoError(t, c.Create(ctx, home))
				r := workspacecontroller.WorkspaceReconciler{Client: c, APIReader: c, Scheme: scheme, RunnerImage: ownershipRunnerImage}
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
				require.NoError(t, err)
				assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(workspace), new(corev1.Pod))), "a late direct-API mount must not create a runtime against a deleting Worktree")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	// ROOT CAUSE: inserting after both mount lists bypassed the finalizer check,
	// and Workspace resolution accepted the still-Ready deleting Worktree.
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
	require.NoError(t, err)
	assert.True(t, inserted, "inject immediately before the actual finalizer removal")
}

// Formerly TestExecCannotStartAfterDeletionLeaseDisappears: foreground GC
// could remove the deletion Lease and let a writer acquire the vacant name.
// Writers now admit by CAS on the Worktree itself, so the equivalent premise is
// that an exec cannot start after Close, even when Close commits between the
// exec's admission read and its write (the WorktreeExec CAS race).
func TestExecCannotStartAfterClose(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, _ := ownershipStorage(t)
	exec := &repositoriesv1alpha1.WorktreeExec{ObjectMeta: metav1.ObjectMeta{Name: "late-exec", Namespace: ownershipNamespace, UID: "late-exec-uid"}, Spec: repositoriesv1alpha1.WorktreeExecSpec{WorktreeRef: repositoriesv1alpha1.WorktreeReference{Name: worktree.Name}, Command: []string{"probe-closed-storage"}}}
	injected := false
	kube := indexedFake(scheme).WithStatusSubresource(exec).WithObjects(worktree, exec).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*repositoriesv1alpha1.Worktree); ok && !injected {
				injected = true
				// ROOT CAUSE: a writer that resolved Ready before the fence must not
				// create its Job once the fence has committed.
				remaining, err := (worktreeownership.MountAccess{Client: c, Reader: c}).Close(ctx, worktree)
				require.NoError(t, err)
				require.Empty(t, remaining)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	r := WorktreeExecReconciler{Client: kube, APIReader: kube, Scheme: scheme, RunnerImage: ownershipRunnerImage}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(exec)})
	require.NoError(t, err)
	require.True(t, injected)
	jobs := new(batchv1.JobList)
	require.NoError(t, kube.List(ctx, jobs))
	assert.Empty(t, jobs.Items, "admission after Close cannot reopen the Worktree")
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	state, err := worktreeownership.Decode(worktree)
	require.NoError(t, err)
	assert.True(t, state.Closed)
	assert.Empty(t, state.Holders, "the rejected exec holds nothing")
}

func TestExecAdmittedBeforeCloseKeepsCleanupWaiting(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, _ := ownershipStorage(t)
	exec := &repositoriesv1alpha1.WorktreeExec{ObjectMeta: metav1.ObjectMeta{Name: "early-exec", Namespace: ownershipNamespace, UID: "early-exec-uid"}, Spec: repositoriesv1alpha1.WorktreeExecSpec{WorktreeRef: repositoriesv1alpha1.WorktreeReference{Name: worktree.Name}, Command: []string{"probe-early-writer"}}}
	kube := indexedFake(scheme).WithStatusSubresource(exec, worktree).WithObjects(worktree, exec).Build()
	r := WorktreeExecReconciler{Client: kube, APIReader: kube, Scheme: scheme, RunnerImage: ownershipRunnerImage}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(exec)})
	require.NoError(t, err)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(exec), new(batchv1.Job)), "the admitted exec created its Job")
	require.NoError(t, kube.Delete(ctx, worktree))
	cleanup := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	result, err := cleanup.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	condition := meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionDeletionBlocked)
	require.NotNil(t, condition)
	assert.Equal(t, repositoriesv1alpha1.DeletionBlockedReasonWaitingForWriter, condition.Reason)
	assert.Contains(t, condition.Message, "WorktreeExec/early-exec")
}
