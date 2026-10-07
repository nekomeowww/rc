package repositories

import (
	"context"
	"fmt"
	"slices"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/nekomeowww/rc/internal/volumeclaim"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

// prepareWorktreeCleanup closes the same CAS gate used before Pod creation.
// The mount list remains a conservative legacy/reference check, not a lock.
// Read-only consumers and in-flight creators are covered by durable holders;
// the ordinary writer Lease continues to serialize Workspace/WorktreeExec use.
// When ready, it returns the resolved PVC name that cleanup must act on.
func (r *WorktreeReconciler) prepareWorktreeCleanup(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) (string, bool, error) {
	log := logf.FromContext(ctx)
	drained, err := (worktreeownership.MountAccess{Client: r.Client, Reader: r.APIReader}).Close(ctx, worktree)
	if err != nil || !drained {
		return "", false, err
	}
	blockers, err := r.cleanupReferenceBlockers(ctx, worktree)
	if err != nil || len(blockers) > 0 {
		return "", false, err
	}
	deletionLease := worktreeclaim.DeletionLease(worktree)
	if err := r.Create(ctx, deletionLease); err != nil {
		if !errors.IsAlreadyExists(err) {
			return "", false, fmt.Errorf("acquire Worktree deletion Lease: %w", err)
		}
		current := new(coordinationv1.Lease)
		if err := r.Get(ctx, client.ObjectKeyFromObject(deletionLease), current); err != nil {
			// Foreground GC can remove the Lease between Create and Get. Requeue
			// to acquire it again; disappearance does not authorize cleanup.
			if errors.IsNotFound(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("get Worktree deletion Lease: %w", err)
		}
		if !worktreeclaim.IsDeletionHolder(worktree, current) {
			log.Info("Worktree deletion is waiting for active writer", "worktree", worktree.Name, "holder", current.Labels[worktreeclaim.HolderLabel])
			return "", false, nil
		}
	}

	// Re-list after acquiring the writer claim for legacy clients. New runtime
	// admissions are fenced atomically, including after this final list.
	blockers, err = r.cleanupReferenceBlockers(ctx, worktree)
	if err != nil || len(blockers) > 0 {
		return "", false, err
	}
	claimName, _, resolveErr := volumeclaim.Resolve(ctx, r.APIReader, worktree, volumeclaim.Worktree, 0, worktree.Status.VolumeClaimName)
	if resolveErr != nil && claimName == "" {
		return "", false, resolveErr
	}
	pods := new(corev1.PodList)
	if err := r.APIReader.List(ctx, pods, client.InNamespace(worktree.Namespace)); err != nil {
		return "", false, err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed && podUsesPersistentVolumeClaim(&pod, claimName) {
			return "", false, nil
		}
	}
	return claimName, true, nil
}

// cleanupReferenceBlockers preserves references when deleting a Worktree. An
// explicit PVC DELETE instead waits for admitted/actual consumers; a suspended
// Workspace's desired mount must not strand an unused PVC in Terminating.
func (r *WorktreeReconciler) cleanupReferenceBlockers(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) ([]string, error) {
	if worktree.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	return worktreeownership.ReferenceBlockers(ctx, r.APIReader, worktree.Namespace, worktree.Name)
}

// reconcileStorageDeletion handles a direct PVC DELETE without deleting its
// live Worktree or silently cloning replacement data. The durable fence remains
// after the PVC disappears, and other Kubernetes storage finalizers stay intact.
func (r *WorktreeReconciler) reconcileStorageDeletion(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) (ctrl.Result, error) {
	claimName, ready, err := r.prepareWorktreeCleanup(ctx, worktree)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: worktreeRequeueDelay}, r.setStorageDeletionStatus(ctx, worktree, "VolumeDeleting", "Storage deletion requested; waiting for mounts, writers and Pods to stop")
	}
	claim := new(corev1.PersistentVolumeClaim)
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: worktree.Namespace, Name: claimName}, claim); err == nil {
		if !metav1.IsControlledBy(claim, worktree) || claim.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, r.setStorageDeletionStatus(ctx, worktree, "VolumeDeletionFenced", "Storage admission is permanently closed; inspect the Worktree before replacing it")
		}
		if err := r.setStorageDeletionStatus(ctx, worktree, "VolumeDeleting", "Storage deletion requested; waiting for PVC cleanup"); err != nil {
			return ctrl.Result{}, err
		}
		if err := worktreeownership.ReleaseVolumeProtection(ctx, r.Client, claim); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: worktreeRequeueDelay}, nil
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err := r.releaseClone(ctx, worktree); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.setStorageDeletionStatus(ctx, worktree, "VolumeDeleted", "Storage was explicitly deleted; this Worktree will not recreate a checkout")
}

func (r *WorktreeReconciler) setStorageDeletionStatus(ctx context.Context, worktree *repositoriesv1alpha1.Worktree, reason, message string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Worktree)
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(worktree), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		before := current.DeepCopy()
		current.Status.ObservedGeneration = current.Generation
		for _, condition := range []string{repositoriesv1alpha1.WorktreeConditionReady, repositoriesv1alpha1.WorktreeConditionVolumeReady} {
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: condition, Status: metav1.ConditionFalse, ObservedGeneration: current.Generation, Reason: reason, Message: message})
		}
		if slices.Equal(current.Status.Conditions, before.Status.Conditions) {
			return nil
		}
		return r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}

// cloneGate admits a Worktree clone against its source Repository.
func (r *WorktreeReconciler) cloneGate() repositoryaccess.Gate {
	return repositoryaccess.Gate{Client: r.Client, Reader: r.APIReader}
}

// cloneToken identifies a Worktree's clone reservation on its Repository.
func cloneToken(worktree *repositoriesv1alpha1.Worktree) string {
	return repositoryaccess.Token("clone", worktree)
}

// releaseClone drops a Worktree's clone reservation. It is idempotent.
func (r *WorktreeReconciler) releaseClone(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) error {
	return r.cloneGate().Release(ctx, worktree.Namespace, cloneToken(worktree))
}
