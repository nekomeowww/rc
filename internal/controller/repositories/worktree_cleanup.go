package repositories

import (
	"context"
	"fmt"
	"slices"
	"strings"

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
	"github.com/nekomeowww/rc/internal/holdset"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/nekomeowww/rc/internal/volumeclaim"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

// cleanupWait explains why Worktree cleanup cannot proceed yet. Its reason is
// one of the DeletionBlockedReason* API constants.
type cleanupWait struct {
	reason  string
	message string
}

// prepareWorktreeCleanup closes the same CAS gate used before Pod creation.
// The mount list remains a conservative legacy/reference check, not a lock.
// Read-only consumers and in-flight creators are covered by durable holders;
// the ordinary writer Lease continues to serialize Workspace/WorktreeExec use.
// When ready (wait is nil), it returns the resolved PVC name that cleanup must
// act on. Otherwise wait names what cleanup is waiting for.
func (r *WorktreeReconciler) prepareWorktreeCleanup(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) (string, *cleanupWait, error) {
	log := logf.FromContext(ctx)
	drained, err := (worktreeownership.MountAccess{Client: r.Client, Reader: r.APIReader}).Close(ctx, worktree)
	if err != nil {
		return "", nil, err
	}
	if !drained {
		return "", &cleanupWait{reason: repositoriesv1alpha1.DeletionBlockedReasonWaitingForMounts, message: "Waiting for admitted Workspace mounts to be released"}, nil
	}
	if wait, err := r.cleanupReferenceWait(ctx, worktree); err != nil || wait != nil {
		return "", wait, err
	}
	deletionLease := worktreeclaim.DeletionLease(worktree)
	if err := r.Create(ctx, deletionLease); err != nil {
		if !errors.IsAlreadyExists(err) {
			return "", nil, fmt.Errorf("acquire Worktree deletion Lease: %w", err)
		}
		current := new(coordinationv1.Lease)
		if err := r.Get(ctx, client.ObjectKeyFromObject(deletionLease), current); err != nil {
			// Foreground GC can remove the Lease between Create and Get. Requeue
			// to acquire it again; disappearance does not authorize cleanup.
			if errors.IsNotFound(err) {
				return "", &cleanupWait{reason: repositoriesv1alpha1.DeletionBlockedReasonWaitingForWriter, message: "Waiting to acquire the Worktree write Lease for deletion"}, nil
			}
			return "", nil, fmt.Errorf("get Worktree deletion Lease: %w", err)
		}
		if !worktreeclaim.IsDeletionHolder(worktree, current) {
			holder := current.Labels[worktreeclaim.HolderLabel]
			log.Info("Worktree deletion is waiting for active writer", "worktree", worktree.Name, "holder", holder)
			return "", &cleanupWait{reason: repositoriesv1alpha1.DeletionBlockedReasonWaitingForWriter, message: fmt.Sprintf("Waiting for writer %q to release the Worktree write Lease %s", holder, current.Name)}, nil
		}
	}

	// Re-list after acquiring the writer claim for legacy clients. New runtime
	// admissions are fenced atomically, including after this final list.
	if wait, err := r.cleanupReferenceWait(ctx, worktree); err != nil || wait != nil {
		return "", wait, err
	}
	claimName, _, resolveErr := volumeclaim.Resolve(ctx, r.APIReader, worktree, volumeclaim.Worktree, 0, worktree.Status.VolumeClaimName)
	if resolveErr != nil && claimName == "" {
		return "", nil, resolveErr
	}
	pods := new(corev1.PodList)
	if err := r.APIReader.List(ctx, pods, client.InNamespace(worktree.Namespace)); err != nil {
		return "", nil, err
	}
	consumers := make([]string, 0)
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed && podUsesPersistentVolumeClaim(&pod, claimName) {
			consumers = append(consumers, pod.Name)
		}
	}
	if len(consumers) > 0 {
		slices.Sort(consumers)
		return "", &cleanupWait{reason: repositoriesv1alpha1.DeletionBlockedReasonWaitingForPods, message: "Waiting for Pods using the Worktree volume to stop: " + strings.Join(consumers, ", ")}, nil
	}
	return claimName, nil, nil
}

// cleanupReferenceWait reports Workspaces whose spec still mounts a deleting
// Worktree.
func (r *WorktreeReconciler) cleanupReferenceWait(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) (*cleanupWait, error) {
	blockers, err := r.cleanupReferenceBlockers(ctx, worktree)
	if err != nil || len(blockers) == 0 {
		return nil, err
	}
	return &cleanupWait{reason: repositoriesv1alpha1.DeletionBlockedReasonWaitingForMounts, message: "Waiting for Workspaces that mount this Worktree: " + strings.Join(blockers, ", ")}, nil
}

// setDeletionBlocked publishes why the deletion finalizer is waiting. A nil
// wait removes the condition. It writes only while the Worktree is deleting.
func (r *WorktreeReconciler) setDeletionBlocked(ctx context.Context, worktree *repositoriesv1alpha1.Worktree, wait *cleanupWait) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Worktree)
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(worktree), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != worktree.UID || current.DeletionTimestamp.IsZero() {
			return nil
		}
		before := current.DeepCopy()
		if wait == nil {
			meta.RemoveStatusCondition(&current.Status.Conditions, repositoriesv1alpha1.WorktreeConditionDeletionBlocked)
		} else {
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type: repositoriesv1alpha1.WorktreeConditionDeletionBlocked, Status: metav1.ConditionTrue,
				ObservedGeneration: current.Generation, Reason: wait.reason, Message: wait.message,
			})
		}
		if slices.Equal(current.Status.Conditions, before.Status.Conditions) {
			return nil
		}
		return client.IgnoreNotFound(r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
	})
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
	claimName, wait, err := r.prepareWorktreeCleanup(ctx, worktree)
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait != nil {
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

// cloneHolder identifies a Worktree's clone reservation on its Repository.
func cloneHolder(worktree *repositoriesv1alpha1.Worktree) holdset.Holder {
	return repositoryaccess.Holder(repositoryaccess.KindWorktree, worktree, repositoryaccess.Clone)
}

// releaseClone drops a Worktree's clone reservation. It is idempotent.
func (r *WorktreeReconciler) releaseClone(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) error {
	key := client.ObjectKey{Namespace: worktree.Namespace, Name: worktree.Spec.RepositoryRef.Name}
	return r.cloneGate().ReleaseNamed(ctx, key, cloneHolder(worktree).Key())
}
