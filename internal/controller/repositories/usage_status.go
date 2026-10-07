package repositories

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/holdset"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

// usageReferences mirrors holders in status, in hold set order.
func usageReferences(holders []holdset.Holder) []repositoriesv1alpha1.UsageReference {
	if len(holders) == 0 {
		return nil
	}
	references := make([]repositoriesv1alpha1.UsageReference, 0, len(holders))
	for _, holder := range holders {
		reference := repositoriesv1alpha1.UsageReference{Kind: holder.Kind, Name: holder.Name, UID: holder.UID, Mode: string(holder.Mode)}
		if !holder.Since.IsZero() {
			reference.Since = holder.Since.DeepCopy()
		}
		references = append(references, reference)
	}
	return references
}

// worktreeInUse derives the InUse condition from holders, then from Pods
// that use the volume outside the hold set.
func worktreeInUse(holders []holdset.Holder, consumers []string) (metav1.ConditionStatus, string, string) {
	names := make([]string, 0, len(holders))
	reason := ""
	for _, holder := range holders {
		names = append(names, fmt.Sprintf("%s/%s (%s)", holder.Kind, holder.Name, holder.Mode))
		switch {
		case holder.Kind == worktreeownership.KindWorkspace:
			reason = repositoriesv1alpha1.WorktreeReasonMountedByWorkspace
		case reason == "" && holder.Kind == worktreeownership.KindWorktreeExec:
			reason = repositoriesv1alpha1.WorktreeReasonWriterExec
		}
	}
	if len(holders) > 0 {
		if reason == "" {
			reason = repositoriesv1alpha1.WorktreeReasonMountedByWorkspace
		}
		return metav1.ConditionTrue, reason, "Used by " + strings.Join(names, ", ")
	}
	if len(consumers) > 0 {
		return metav1.ConditionTrue, repositoriesv1alpha1.WorktreeReasonPodConsumer, "Pods use the volume: " + strings.Join(consumers, ", ")
	}
	return metav1.ConditionFalse, repositoriesv1alpha1.WorktreeReasonIdle, "Nothing uses the Worktree"
}

// publishUsage mirrors the hold set in status.usedBy and InUse. It runs after
// the other status writers, reads outside the cache, and writes only on change.
func (r *WorktreeReconciler) publishUsage(ctx context.Context, key client.ObjectKey) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Worktree)
		if err := r.APIReader.Get(ctx, key, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		state, err := worktreeownership.Decode(current)
		if err != nil {
			return err
		}
		consumers := make([]string, 0)
		if len(state.Holders) == 0 && current.Status.VolumeClaimName != "" {
			pods := new(corev1.PodList)
			if err := r.List(ctx, pods, client.InNamespace(current.Namespace)); err != nil {
				return fmt.Errorf("list Pods using the Worktree volume: %w", err)
			}
			for index := range pods.Items {
				pod := &pods.Items[index]
				if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed && podUsesPersistentVolumeClaim(pod, current.Status.VolumeClaimName) {
					consumers = append(consumers, pod.Name)
				}
			}
		}
		before := current.DeepCopy()
		current.Status.UsedBy = usageReferences(state.Holders)
		status, reason, message := worktreeInUse(state.Holders, consumers)
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: repositoriesv1alpha1.WorktreeConditionInUse, Status: status,
			ObservedGeneration: current.Generation, Reason: reason, Message: message,
		})
		if equality.Semantic.DeepEqual(before.Status, current.Status) {
			return nil
		}
		return client.IgnoreNotFound(r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
	})
}

// publishAccess mirrors the Repository's reservations in status.access. It
// reads outside the cache and writes only on change.
func (r *RepositoryReconciler) publishAccess(ctx context.Context, key client.ObjectKey) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Repository)
		if err := r.APIReader.Get(ctx, key, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		_, state, err := (repositoryaccess.Gate{Client: r.Client, Reader: r.APIReader}).Store(current).Load(ctx)
		if err != nil {
			return err
		}
		before := current.DeepCopy()
		current.Status.Access = nil
		if len(state.Holders) > 0 {
			current.Status.Access = &repositoriesv1alpha1.RepositoryAccessStatus{Mode: string(state.Holders[0].Mode), Holders: usageReferences(state.Holders)}
		}
		if equality.Semantic.DeepEqual(before.Status, current.Status) {
			return nil
		}
		return client.IgnoreNotFound(r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
	})
}
