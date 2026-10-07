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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/volumeclaim"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
	"github.com/nekomeowww/rc/internal/worktreeownership"
	"github.com/nekomeowww/rc/internal/worktreestorage"
)

const (
	ownershipRunnerImage  = "ownership-runner:test"
	ownershipStorageClass = "standard"
)

func ownershipScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, coordinationv1.AddToScheme, repositoriesv1alpha1.AddToScheme, workspacesv1alpha1.AddToScheme} {
		require.NoError(t, add(scheme))
	}
	return scheme
}

func ownershipStorage(t *testing.T) (*repositoriesv1alpha1.Repository, *repositoriesv1alpha1.Worktree, *corev1.PersistentVolumeClaim) {
	t.Helper()
	repository := &repositoriesv1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: "ownership-parent", Namespace: ownershipNamespace}, Spec: repositoriesv1alpha1.RepositorySpec{Storage: repositoriesv1alpha1.RepositoryStorageSpec{Size: resource.MustParse("1Gi"), StorageClassName: ownershipStorageClass}}, Status: repositoriesv1alpha1.RepositoryStatus{VolumeClaimName: "ownership-parent"}}
	claimName := volumeclaim.Name(volumeclaim.Worktree, ownershipWorktreeName, 0)
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorktreeName, Namespace: ownershipNamespace, UID: ownershipWorktreeUID, Finalizers: []string{worktreeDeletionFinalizer}, Labels: map[string]string{worktreeownership.GeneratedForLabel: ownershipWorkspaceName}}, Spec: repositoriesv1alpha1.WorktreeSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repository.Name}, Branch: "feature"}, Status: repositoriesv1alpha1.WorktreeStatus{VolumeClaimName: claimName, Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue}, {Type: repositoriesv1alpha1.WorktreeConditionVolumeReady, Status: metav1.ConditionTrue}}}}
	claim := worktreeVolumeClaim(worktree, claimName, repository.Name, worktreestorage.Plan{StorageClassName: ownershipStorageClass, Size: resource.MustParse("1Gi"), AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}})
	require.NoError(t, controllerutil.SetControllerReference(worktree, claim, ownershipScheme(t)))
	claim.Status.Phase = corev1.ClaimBound
	return repository, worktree, claim
}

func TestDirectLiveWorktreePVCDeletionConverges(t *testing.T) {
	ctx := t.Context()
	repository, worktree, claim := ownershipStorage(t)
	scheme := ownershipScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree, claim).WithObjects(repository, worktree, claim).Build()
	require.NoError(t, kube.Delete(ctx, claim))
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	// ROOT CAUSE: the live Worktree path skipped terminating PVCs in
	// EnsureVolumeProtection and kept reporting Ready, with no finalizer owner
	// ever releasing the storage guard. A delete must fence use and converge.
	for range 3 {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
		require.NoError(t, err)
	}
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(claim), new(corev1.PersistentVolumeClaim))), "unused directly deleted PVC must finish deleting")
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	assert.False(t, meta.IsStatusConditionTrue(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady))
	assert.True(t, worktree.DeletionTimestamp.IsZero(), "a storage delete must not implicitly delete the independent Worktree")
}

func TestDirectPVCDeletionWaitsForConsumerAndNeverReclones(t *testing.T) {
	ctx := t.Context()
	_, worktree, claim := ownershipStorage(t)
	scheme := ownershipScheme(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "external-reader", Namespace: ownershipNamespace}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name, ReadOnly: true}}}}}}
	// The Repository is intentionally absent. Storage cleanup must remain reachable.
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree, claim).WithObjects(worktree, claim, pod).Build()
	require.NoError(t, kube.Delete(ctx, claim))
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	current := new(corev1.PersistentVolumeClaim)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(claim), current))
	assert.Contains(t, current.Finalizers, worktreeownership.VolumeProtectionFinalizer, "do not remove protection from an actual consumer")
	require.NoError(t, kube.Delete(ctx, pod))
	for range 3 {
		_, err = r.Reconcile(ctx, req)
		require.NoError(t, err)
	}
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(claim), current)))
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	require.Equal(t, "VolumeDeleted", meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady).Reason)
	version := worktree.ResourceVersion
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	assert.Equal(t, version, worktree.ResourceVersion, "terminal storage deletion must not cause status update loops")
}

func TestDirectPVCDeletionIgnoresUnadmittedDesiredMount(t *testing.T) {
	ctx := t.Context()
	_, worktree, claim := ownershipStorage(t)
	scheme := ownershipScheme(t)
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "suspended-consumer", Namespace: ownershipNamespace}, Spec: workspacesv1alpha1.WorkspaceSpec{DesiredState: workspacesv1alpha1.WorkspaceDesiredStateSuspended, Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: ownershipWorktreeName, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}, ReadOnly: true}}}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree, claim).WithObjects(worktree, claim, workspace).Build()
	require.NoError(t, kube.Delete(ctx, claim))
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	for range 3 {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
		require.NoError(t, err)
	}
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(claim), new(corev1.PersistentVolumeClaim))))
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	assert.True(t, worktreeownership.MountsClosed(worktree))
}

const (
	ownershipNamespace     = "test"
	ownershipWorktreeName  = "code"
	ownershipWorkspaceName = "owner"
	ownershipWorkspaceUID  = "owner-uid"
	ownershipWorktreeUID   = "code-uid"
)

func TestDeletingWorkspaceDoesNotBlockWorktreeGC(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	now := metav1.Now()
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorkspaceName, Namespace: ownershipNamespace, UID: ownershipWorkspaceUID, DeletionTimestamp: &now, Finalizers: []string{metav1.FinalizerDeleteDependents}}, Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: ownershipWorktreeName, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: ownershipWorktreeName}}}}}
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorktreeName, Namespace: ownershipNamespace, UID: ownershipWorktreeUID, DeletionTimestamp: &now, Finalizers: []string{worktreeDeletionFinalizer}}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, worktree).Build()
	r := &WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	// ROOT CAUSE: foreground GC waits for the dependent's finalizer, while
	// the finalizer counted its deleting owner as a live reference forever.
	blockers, err := r.cleanupReferenceBlockers(context.Background(), worktree)
	require.NoError(t, err)
	assert.Empty(t, blockers)
	result, err := r.reconcileDelete(context.Background(), worktree)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
}

func TestWorktreeGCWaitsForRuntimeCleanupAndWriterThenFinishes(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	now := metav1.Now()
	owner := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorkspaceName, Namespace: ownershipNamespace, UID: ownershipWorkspaceUID, DeletionTimestamp: &now, Finalizers: []string{worktreeownership.WorkspaceCleanupFinalizer, metav1.FinalizerDeleteDependents}}, Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: ownershipWorktreeName, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: ownershipWorktreeName}}}}}
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorktreeName, Namespace: ownershipNamespace, UID: ownershipWorktreeUID, DeletionTimestamp: &now, Finalizers: []string{worktreeDeletionFinalizer}}}
	holder := string(owner.UID)
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: worktreeclaim.LeaseName(worktree), Namespace: ownershipNamespace}, Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, worktree, lease).Build()
	r := &WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	blockers, err := r.cleanupReferenceBlockers(ctx, worktree)
	require.NoError(t, err)
	assert.Equal(t, []string{owner.Name}, blockers, "runtime teardown still protects read-only and hot mounts even if GC removes writer Leases")
	owner.Finalizers = []string{metav1.FinalizerDeleteDependents}
	require.NoError(t, kube.Update(ctx, owner))
	result, err := r.reconcileDelete(ctx, worktree)
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter, "an independent writer must still finish")
	require.NoError(t, kube.Delete(ctx, lease))
	result, err = r.reconcileDelete(ctx, worktree)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	require.True(t, apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(worktree), new(repositoriesv1alpha1.Worktree))))
}

func TestWorktreeVolumeIsProtectedFromFirstCreation(t *testing.T) {
	claim := worktreeVolumeClaim(&repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorktreeName, Namespace: ownershipNamespace}}, volumeclaim.Name(volumeclaim.Worktree, ownershipWorktreeName, 0), "repo", worktreestorage.Plan{StorageClassName: ownershipStorageClass, Size: resource.MustParse("1Gi"), AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}})
	assert.Contains(t, claim.Finalizers, worktreeownership.VolumeProtectionFinalizer)
}

func TestWorktreeDeletionFinishesWhenGCRacesFinalizerPatch(t *testing.T) {
	ctx := t.Context()
	_, worktree, _ := ownershipStorage(t)
	now := metav1.Now()
	worktree.DeletionTimestamp = &now
	injected := false
	kube := fake.NewClientBuilder().WithScheme(ownershipScheme(t)).WithObjects(worktree).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if current, ok := obj.(*repositoriesv1alpha1.Worktree); ok && !controllerutil.ContainsFinalizer(current, worktreeDeletionFinalizer) {
				// ROOT CAUSE: real foreground GC can finish deletion between the final
				// GET and PATCH. NotFound means cleanup already converged, not failure.
				injected = true
				latest := new(repositoriesv1alpha1.Worktree)
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(current), latest))
				latest.Finalizers = nil
				require.NoError(t, c.Update(ctx, latest))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: kube.Scheme()}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
	require.NoError(t, err)
	assert.True(t, injected)
	assert.Zero(t, result.RequeueAfter)
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(worktree), new(repositoriesv1alpha1.Worktree))))
}

func TestWorktreeCleanupRetriesWhenDeletionLeaseDisappears(t *testing.T) {
	ctx := t.Context()
	_, worktree, _ := ownershipStorage(t)
	now := metav1.Now()
	worktree.DeletionTimestamp = &now
	lease := worktreeclaim.DeletionLease(worktree)
	injected := false
	kube := fake.NewClientBuilder().WithScheme(ownershipScheme(t)).WithObjects(worktree, lease).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*coordinationv1.Lease); ok && !injected {
				// ROOT CAUSE: foreground GC may delete the Lease after Create returns
				// AlreadyExists. Cleanup must requeue without bypassing the writer check.
				injected = true
				require.NoError(t, c.Delete(ctx, lease))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: kube.Scheme()}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)}
	result, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.True(t, injected)
	assert.Positive(t, result.RequeueAfter)
	require.NoError(t, kube.Get(ctx, req.NamespacedName, worktree))
	assert.Contains(t, worktree.Finalizers, worktreeDeletionFinalizer)
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, req.NamespacedName, new(repositoriesv1alpha1.Worktree))))
}
