package repositories

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	workspacecontroller "github.com/nekomeowww/rc/internal/controller/workspaces"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

func TestMountInsertedAfterFinalDeletionListIsNeverAdmitted(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, _ := ownershipStorage(t)
	now := metav1.Now()
	worktree.DeletionTimestamp = &now
	inserted := false
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree, &workspacesv1alpha1.Workspace{}, &corev1.PersistentVolumeClaim{}).WithObjects(worktree).WithInterceptorFuncs(interceptor.Funcs{
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

func TestExecCannotStartAfterDeletionLeaseDisappears(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, _ := ownershipStorage(t)
	exec := &repositoriesv1alpha1.WorktreeExec{ObjectMeta: metav1.ObjectMeta{Name: "late-exec", Namespace: ownershipNamespace, UID: "late-exec-uid"}, Spec: repositoriesv1alpha1.WorktreeExecSpec{WorktreeRef: repositoriesv1alpha1.WorktreeReference{Name: worktree.Name}, Command: []string{"probe-closed-storage"}}}
	injected := false
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(exec).WithObjects(worktree, exec).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*coordinationv1.Lease); ok && !injected {
				injected = true
				// ROOT CAUSE: foreground GC can remove the deletion Lease. A writer that
				// resolved Ready before the fence may now acquire the vacant Lease name.
				drained, err := (worktreeownership.MountAccess{Client: c, Reader: c}).Close(ctx, worktree)
				require.NoError(t, err)
				require.True(t, drained)
				lease := worktreeclaim.DeletionLease(worktree)
				require.NoError(t, c.Create(ctx, lease))
				require.NoError(t, c.Delete(ctx, lease))
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	r := WorktreeExecReconciler{Client: kube, APIReader: kube, Scheme: scheme, RunnerImage: ownershipRunnerImage}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(exec)})
	require.NoError(t, err)
	require.True(t, injected)
	jobs := new(batchv1.JobList)
	require.NoError(t, kube.List(ctx, jobs))
	assert.Empty(t, jobs.Items, "acquiring a recreated Lease cannot reopen mount admission")
	leases := new(coordinationv1.LeaseList)
	require.NoError(t, kube.List(ctx, leases))
	assert.Empty(t, leases.Items, "rejected exec releases its unused claim")
}
