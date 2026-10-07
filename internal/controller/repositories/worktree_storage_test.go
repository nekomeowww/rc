package repositories

import (
	"context"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/nekomeowww/rc/internal/volumeclaim"
	"github.com/nekomeowww/rc/internal/worktreestorage"
)

// Shared identities for creation and restart-recovery fixtures.
const (
	cloneStorageTestClass             = "tns-iscsi"
	cloneStorageTestParent            = "parent"
	cloneStorageTestReplacement       = "replacement-parent"
	cloneStorageTestSpecChangedReason = "VolumeClaimSpecChanged"
)

func cloneStorageFixture(t *testing.T) (client.WithWatch, *repositoriesv1alpha1.Repository, *corev1.PersistentVolumeClaim, *repositoriesv1alpha1.Worktree) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: cloneStorageTestParent, Namespace: metav1.NamespaceDefault, UID: "parent-uid", Generation: 1},
		Spec:       repositoriesv1alpha1.RepositorySpec{Storage: repositoriesv1alpha1.RepositoryStorageSpec{StorageClassName: cloneStorageTestClass, Size: resource.MustParse("60Gi")}},
		Status:     repositoriesv1alpha1.RepositoryStatus{VolumeClaimName: cloneStorageTestParent, ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}}},
	}
	class := cloneStorageTestClass
	source := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: cloneStorageTestParent, Namespace: metav1.NamespaceDefault},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("60Gi")}}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("60Gi")}},
	}
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-storage-child", Namespace: metav1.NamespaceDefault, UID: "child-uid", Generation: 1},
		Spec:       repositoriesv1alpha1.WorktreeSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: cloneStorageTestParent}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree, source, repository).WithObjects(repository, source, worktree).Build()
	return c, repository, source, worktree
}

// TestWorktreeCloneRecoversSourceAfterStatusWriteFailure reproduces the durable
// PVC / missing Worktree status window, then restarts with a different parent.
func TestWorktreeCloneRecoversSourceAfterStatusWriteFailure(t *testing.T) {
	for _, phase := range []corev1.PersistentVolumeClaimPhase{corev1.ClaimPending, corev1.ClaimBound} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := t.Context()
			c, repository, source, worktree := cloneStorageFixture(t)
			// A source observation alone is not evidence that creation committed.
			worktree.Status.SourceVolumeClaimName = "previously-observed-parent"
			require.NoError(t, c.Status().Update(ctx, worktree))
			key := client.ObjectKeyFromObject(worktree)
			stoppedBeforeStatus := errors.New("controller stopped before Worktree status write")
			statusWrites := 0
			// ROOT CAUSE:
			// PVC Create commits the immutable clone source before Worktree status is
			// written. A restart can therefore observe a child with no status source.
			// Falling back to the current Repository source rewrites history and makes
			// the legitimate child permanently fail VolumeClaimSpecChanged.
			failingClient := interceptor.NewClient(c, interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, kubeClient client.Client, subresource string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if _, ok := object.(*repositoriesv1alpha1.Worktree); ok && subresource == "status" {
						statusWrites++
						return stoppedBeforeStatus
					}
					return kubeClient.SubResource(subresource).Patch(ctx, object, patch, opts...)
				},
			})
			first := &WorktreeReconciler{Client: failingClient, Scheme: c.Scheme()}
			_, err := first.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			require.ErrorIs(t, err, stoppedBeforeStatus)
			require.Equal(t, 1, statusWrites)
			claim := new(corev1.PersistentVolumeClaim)
			claimKey := client.ObjectKey{Namespace: worktree.Namespace, Name: volumeclaim.Name(volumeclaim.Worktree, worktree.Name, 0)}
			require.NoError(t, c.Get(ctx, claimKey, claim), "PVC creation must have succeeded before the simulated restart")
			require.True(t, metav1.IsControlledBy(claim, worktree))
			require.Equal(t, source.Name, claim.Spec.DataSource.Name)
			require.NoError(t, c.Get(ctx, key, worktree))
			require.Equal(t, "previously-observed-parent", worktree.Status.SourceVolumeClaimName)
			require.Empty(t, worktree.Status.VolumeClaimName)
			stale := worktree.DeepCopy()

			repository.Status.VolumeClaimName = cloneStorageTestReplacement
			require.NoError(t, c.Status().Update(ctx, repository))
			claim.Status.Phase = phase
			require.NoError(t, c.Status().Update(ctx, claim))
			restarted := &WorktreeReconciler{Client: c, Scheme: c.Scheme()}
			for range 2 {
				_, err = restarted.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				require.NoError(t, err)
				require.NoError(t, c.Get(ctx, key, worktree))
				condition := meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
				require.NotNil(t, condition)
				wantReason := "Provisioning"
				if phase == corev1.ClaimBound {
					wantReason = "Initializing"
				}
				assert.Equal(t, wantReason, condition.Reason)
				assert.Equal(t, source.Name, worktree.Status.SourceVolumeClaimName, "persist the child PVC's committed creation-time source")
				assert.Equal(t, claim.Name, worktree.Status.VolumeClaimName)
			}
			persisted := new(corev1.PersistentVolumeClaim)
			require.NoError(t, c.Get(ctx, claimKey, persisted))
			assert.Equal(t, claim.Spec, persisted.Spec, "recovery must not rewrite or replace the independent child")
			busy, err := (repositoryaccess.Gate{Client: c}).Busy(ctx, repository, "another-operation")
			require.NoError(t, err)
			assert.Equal(t, phase != corev1.ClaimBound, busy, "retain original admission until Bound, then release it despite the parent change")

			// ROOT CAUSE:
			// A lost child must not authorize a fresh clone from today's parent.
			// The cache may still show pre-creation status, so both creation evidence
			// and child existence must come from the live reader after a restart.
			require.NoError(t, c.Delete(ctx, persisted))
			replacement := source.DeepCopy()
			replacement.Name, replacement.ResourceVersion = cloneStorageTestReplacement, ""
			require.NoError(t, c.Create(ctx, replacement))
			cached := interceptor.NewClient(c, interceptor.Funcs{
				Get: func(ctx context.Context, kubeClient client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if result, ok := object.(*repositoriesv1alpha1.Worktree); ok {
						stale.DeepCopyInto(result)
						return nil
					}
					return kubeClient.Get(ctx, key, object, opts...)
				},
			})
			restarted = &WorktreeReconciler{Client: cached, APIReader: c, Scheme: c.Scheme()}
			_, err = restarted.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			require.NoError(t, err)
			assert.True(t, apierrors.IsNotFound(c.Get(ctx, key, new(corev1.PersistentVolumeClaim))))
			require.NoError(t, c.Get(ctx, key, worktree))
			ready := meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, "VolumeClaimLost", ready.Reason)
			assert.Equal(t, source.Name, worktree.Status.SourceVolumeClaimName)
			assert.True(t, meta.IsStatusConditionFalse(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionVolumeReady))
			busy, err = (repositoryaccess.Gate{Client: c}).Busy(ctx, repository, "another-operation")
			require.NoError(t, err)
			assert.False(t, busy, "a lost child must release its reservation")
		})
	}
}

// TestWorktreeCloneRecoveryUsesCommittedClaim checks controller orchestration.
// The dataSource/dataSourceRef field matrix belongs in worktreestorage/source_test.go.
func TestWorktreeCloneRecoveryUsesCommittedClaim(t *testing.T) {
	for _, tt := range []struct {
		name              string
		configure         func(*corev1.PersistentVolumeClaim)
		replaceRepository bool
		wantReason        string
	}{
		{name: "Repository removed"},
		{name: "Repository replaced", replaceRepository: true},
		{name: "malformed source", configure: func(claim *corev1.PersistentVolumeClaim) { claim.Spec.DataSource = nil }, wantReason: cloneStorageTestSpecChangedReason},
		{name: "owner incarnation mismatch", configure: func(claim *corev1.PersistentVolumeClaim) { claim.OwnerReferences[0].UID = "previous-worktree" }, wantReason: "VolumeClaimConflict"},
		{name: "explicit storage conflict", configure: func(claim *corev1.PersistentVolumeClaim) {
			class := "unexpected-class"
			claim.Spec.StorageClassName = &class
		}, wantReason: cloneStorageTestSpecChangedReason},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			c, repository, source, worktree := cloneStorageFixture(t)
			worktree.Spec.Storage = &repositoriesv1alpha1.WorktreeStorageSpec{StorageClassName: cloneStorageTestClass}
			require.NoError(t, c.Update(ctx, worktree))
			// Legacy children keep their creation-time RWX default even on an RWO
			// source. Only explicit Worktree storage constrains an existing child.
			plan := worktreestorage.Plan{StorageClassName: cloneStorageTestClass, Size: resource.MustParse("60Gi"), AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, VolumeMode: corev1.PersistentVolumeFilesystem}
			claimName := volumeclaim.Name(volumeclaim.Worktree, worktree.Name, 0)
			claim := worktreeVolumeClaim(worktree, claimName, source.Name, plan)
			require.NoError(t, controllerutil.SetControllerReference(worktree, claim, c.Scheme()))
			claim.Status.Phase = corev1.ClaimBound
			if tt.configure != nil {
				tt.configure(claim)
			}
			require.NoError(t, c.Create(ctx, claim))
			// Recovery starts from independently persisted objects; incorrect status
			// from an earlier controller must not override the child's source record.
			worktree.Status.SourceVolumeClaimName = cloneStorageTestReplacement
			require.NoError(t, c.Status().Update(ctx, worktree))
			require.NoError(t, c.Delete(ctx, repository))
			require.NoError(t, c.Delete(ctx, source))
			if tt.replaceRepository {
				repository.UID, repository.ResourceVersion = "new-repository-incarnation", ""
				repository.Spec.Storage.Size = resource.MustParse("90Gi")
				repository.Spec.Storage.StorageClassName = "new-parent-class"
				repository.Status.VolumeClaimName = cloneStorageTestReplacement
				require.NoError(t, c.Create(ctx, repository))
			}
			r := &WorktreeReconciler{Client: c, Scheme: c.Scheme()}
			key := client.ObjectKeyFromObject(worktree)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			require.NoError(t, err)
			require.NoError(t, c.Get(ctx, key, worktree))
			ready := meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
			require.NotNil(t, ready)
			jobs := new(batchv1.JobList)
			require.NoError(t, c.List(ctx, jobs, client.InNamespace(worktree.Namespace)))
			if tt.wantReason != "" {
				assert.Equal(t, tt.wantReason, ready.Reason)
				assert.Empty(t, jobs.Items, "invalid or unowned storage must not be bootstrapped")
			} else {
				assert.Equal(t, "Initializing", ready.Reason)
				assert.Equal(t, source.Name, worktree.Status.SourceVolumeClaimName)
				assert.Equal(t, claim.Name, worktree.Status.VolumeClaimName)
				assert.True(t, meta.IsStatusConditionTrue(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionVolumeReady))
				assert.Len(t, jobs.Items, 1)
			}
			persisted := new(corev1.PersistentVolumeClaim)
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(claim), persisted))
			assert.Equal(t, claim.Spec, persisted.Spec, "recovery must not rewrite the child")
		})
	}
}
