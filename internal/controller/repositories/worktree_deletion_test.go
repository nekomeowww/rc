package repositories

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/nekomeowww/rc/internal/volumeclaim"
	"github.com/nekomeowww/rc/internal/worktreestorage"
)

func TestWorktreeDeletionWaitsForPendingClaim(t *testing.T) {
	t.Parallel()
	typedName := volumeclaim.Name(volumeclaim.Worktree, testWorktreeName, 0)
	for _, scenario := range []struct {
		name      string
		claimName string
		recorded  bool
	}{
		{name: "typed-recorded", claimName: typedName, recorded: true},
		{name: "typed-before-status-write", claimName: typedName},
		{name: "legacy-recorded", claimName: "retained-checkout", recorded: true},
		{name: "legacy-before-status-write", claimName: testWorktreeName},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			kube, repository, _ := syncFixture(t)
			worktree := &repositoriesv1alpha1.Worktree{
				ObjectMeta: metav1.ObjectMeta{Name: testWorktreeName, Namespace: repository.Namespace, UID: "pending-worktree", Finalizers: []string{worktreeDeletionFinalizer}},
				Spec:       repositoriesv1alpha1.WorktreeSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repository.Name}},
			}
			if scenario.recorded {
				worktree.Status.VolumeClaimName = scenario.claimName
			}
			require.NoError(t, kube.Create(t.Context(), worktree))
			gate := repositoryaccess.Gate{Client: kube, Reader: kube}
			admission, err := gate.Acquire(t.Context(), repository, repositoryaccess.Token("clone", worktree), repositoryaccess.Clone, true)
			require.NoError(t, err)
			require.Equal(t, repositoryaccess.Admitted, admission)
			claim := worktreeVolumeClaim(worktree, scenario.claimName, repository.Status.VolumeClaimName, worktreestorage.Plan{StorageClassName: repository.Spec.Storage.StorageClassName, Size: repository.Spec.Storage.Size, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}})
			require.NoError(t, controllerutil.SetControllerReference(worktree, claim, kube.Scheme()))
			claim.Status.Phase = corev1.ClaimPending
			require.NoError(t, kube.Create(t.Context(), claim))
			if scenario.claimName != typedName && scenario.recorded {
				// A Bound typed-name decoy must not override the recorded legacy claim.
				decoy := claim.DeepCopy()
				decoy.Name, decoy.ResourceVersion, decoy.UID = typedName, "", ""
				decoy.Status.Phase = corev1.ClaimBound
				require.NoError(t, kube.Create(t.Context(), decoy))
			}
			require.NoError(t, kube.Delete(t.Context(), worktree))
			reconciler := &WorktreeReconciler{Client: kube, APIReader: kube, Scheme: kube.Scheme()}
			request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(worktree)}

			// ROOT CAUSE:
			// Deletion looked up the CR name instead of the selected PVC reference.
			// A Pending typed PVC therefore appeared absent, releasing its clone
			// reservation and finalizer while CSI could still be copying the source.
			for range 2 {
				result, err := reconciler.Reconcile(t.Context(), request)
				require.NoError(t, err)
				assert.Positive(t, result.RequeueAfter, "wait for the actual Pending clone")
				busy, err := gate.Busy(t.Context(), repository, "other-writer")
				require.NoError(t, err)
				assert.True(t, busy, "retain the clone reservation while its PVC is Pending")
				current := new(repositoriesv1alpha1.Worktree)
				require.NoError(t, kube.Get(t.Context(), request.NamespacedName, current), "retain the deleting Worktree")
				require.Contains(t, current.Finalizers, worktreeDeletionFinalizer)
				require.Equal(t, worktree.Status.VolumeClaimName, current.Status.VolumeClaimName, "deletion must not replace a recorded legacy reference")
			}

			claim.Status.Phase = corev1.ClaimBound
			require.NoError(t, kube.Status().Update(t.Context(), claim))
			result, err := reconciler.Reconcile(t.Context(), request)
			require.NoError(t, err)
			assert.Zero(t, result.RequeueAfter)
			busy, err := gate.Busy(t.Context(), repository, "other-writer")
			require.NoError(t, err)
			assert.False(t, busy, "release the source reservation after clone completion")
			require.True(t, apierrors.IsNotFound(kube.Get(t.Context(), request.NamespacedName, new(repositoriesv1alpha1.Worktree))), "remove the finalizer after clone completion")
		})
	}
}
