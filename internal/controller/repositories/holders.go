package repositories

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/holdset"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

// holderOwner returns an empty object of a holder's kind, or nil for a kind
// this controller cannot look up. Unknown kinds are never treated as gone.
func holderOwner(kind string) client.Object {
	switch kind {
	case worktreeownership.KindWorkspace:
		return new(workspacesv1alpha1.Workspace)
	case worktreeownership.KindWorktreeExec:
		return new(repositoriesv1alpha1.WorktreeExec)
	case repositoryaccess.KindWorktree:
		return new(repositoriesv1alpha1.Worktree)
	case repositoryaccess.KindRepositorySync:
		return new(repositoriesv1alpha1.RepositorySync)
	case repositoryaccess.KindRepositoryExec:
		return new(repositoriesv1alpha1.RepositoryExec)
	case repositoryaccess.KindRepository:
		return new(repositoriesv1alpha1.Repository)
	default:
		return nil
	}
}

// holderOwnerGone reports whether the incarnation that admitted holder no
// longer exists, read outside the cache. A same-name replacement is gone too.
func holderOwnerGone(ctx context.Context, reader client.Reader, namespace string, holder holdset.Holder) (bool, error) {
	owner := holderOwner(holder.Kind)
	if owner == nil || holder.UID == "" {
		return false, nil
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: holder.Name}, owner); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return owner.GetUID() != holder.UID, nil
}

// claimInUse reports whether any non-terminal Pod mounts claimName, read
// outside the cache. Pods are not attributed to holders: while any consumer
// runs, no holder of that volume is considered stale.
func claimInUse(ctx context.Context, reader client.Reader, namespace, claimName string) (bool, error) {
	if claimName == "" {
		return false, nil
	}
	pods := new(corev1.PodList)
	if err := reader.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return false, fmt.Errorf("list Pods before sweeping holders: %w", err)
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed && podUsesPersistentVolumeClaim(pod, claimName) {
			return true, nil
		}
	}
	return false, nil
}

// reconcileWorktreeHolders imports a legacy write Lease and sweeps stale
// holders. It reports whether the Worktree changed. A stale holder kept for a
// running Pod is swept again when that Pod changes: Pods using the volume
// enqueue this Worktree.
func (r *WorktreeReconciler) reconcileWorktreeHolders(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) (bool, error) {
	imported, err := r.importLegacyWriter(ctx, worktree)
	if err != nil {
		return false, err
	}
	swept, err := r.sweepWorktreeHolders(ctx, worktree)
	return imported || swept, err
}

// importLegacyWriter migrates a write Lease created by an older controller or
// rcctl into the hold set. It imports only while the holder still wants the
// Worktree: a live, non-terminal WorktreeExec, or a live Workspace whose spec
// mounts it writable. Those owners reconcile on Worktree changes and release
// the holder themselves. Any other live Lease keeps blocking writers until
// garbage collection or its owner removes it; a deletion Lease is handled by
// cleanup. The holder is recorded before the Lease is deleted, so a writer is
// never invisible to admission.
func (r *WorktreeReconciler) importLegacyWriter(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) (bool, error) {
	legacy, err := worktreeownership.LegacyWriterOf(ctx, r.APIReader, worktree)
	if err != nil || legacy == nil || legacy.Deletion || legacy.Holder == "" {
		return false, err
	}
	reference := metav1.GetControllerOf(legacy.Lease)
	if reference == nil || reference.UID != legacy.Holder {
		return false, nil
	}
	holder := holdset.Holder{Kind: reference.Kind, Name: reference.Name, UID: reference.UID, Mode: worktreeownership.Write, Since: legacy.Lease.CreationTimestamp}
	wanted, err := r.legacyWriterWanted(ctx, worktree, holder)
	if err != nil || !wanted {
		return false, err
	}
	store := (worktreeownership.MountAccess{Client: r.Client, Reader: r.APIReader}).Store(client.ObjectKeyFromObject(worktree), worktree.UID)
	imported, held := false, false
	if _, err := holdset.Update(ctx, store, func(obj client.Object, state *holdset.State) (bool, error) {
		imported, held = false, false
		if obj == nil {
			return false, nil
		}
		if existing, found := state.Find(holder.Key()); found && existing.Mode == worktreeownership.Write {
			held = true
			return false, nil
		}
		others := make([]holdset.Holder, 0, len(state.Holders))
		for _, existing := range state.Holders {
			if existing.Key() != holder.Key() {
				others = append(others, existing)
			}
		}
		// An older rcctl may have reserved a Worktree that a new writer already
		// holds. That reservation never granted access; leave it blocking.
		if len(worktreeownership.Conflicts(others, holder)) > 0 {
			return false, nil
		}
		// Recording an existing writer is not an admission: a Closed set
		// must still see it, so the fence is not consulted here.
		state.Put(holder)
		imported, held = true, true
		return true, nil
	}); err != nil {
		return false, fmt.Errorf("import legacy Worktree writer: %w", err)
	}
	if !held {
		return false, nil
	}
	if err := r.Delete(ctx, legacy.Lease, client.Preconditions{UID: &legacy.Lease.UID, ResourceVersion: &legacy.Lease.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return imported, fmt.Errorf("delete imported legacy Worktree write Lease: %w", err)
	}
	if imported {
		logf.FromContext(ctx).Info("Imported legacy Worktree write Lease", "worktree", worktree.Name, "lease", legacy.Lease.Name, "holder", holder.Kind+"/"+holder.Name)
	}
	return imported, nil
}

// legacyWriterWanted reports whether a legacy Lease's owner is live and still
// asks to write this Worktree.
func (r *WorktreeReconciler) legacyWriterWanted(ctx context.Context, worktree *repositoriesv1alpha1.Worktree, holder holdset.Holder) (bool, error) {
	key := client.ObjectKey{Namespace: worktree.Namespace, Name: holder.Name}
	switch holder.Kind {
	case worktreeownership.KindWorkspace:
		workspace := new(workspacesv1alpha1.Workspace)
		if err := r.APIReader.Get(ctx, key, workspace); err != nil || workspace.UID != holder.UID {
			return false, client.IgnoreNotFound(err)
		}
		for _, mount := range workspace.Spec.Mounts {
			if mount.WorktreeRef != nil && mount.WorktreeRef.Name == worktree.Name && !mount.ReadOnly {
				return true, nil
			}
		}
		return false, nil
	case worktreeownership.KindWorktreeExec:
		exec := new(repositoriesv1alpha1.WorktreeExec)
		if err := r.APIReader.Get(ctx, key, exec); err != nil || exec.UID != holder.UID {
			return false, client.IgnoreNotFound(err)
		}
		condition := meta.FindStatusCondition(exec.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded)
		return exec.Spec.WorktreeRef.Name == worktree.Name && (condition == nil || condition.Status == metav1.ConditionUnknown), nil
	default:
		return false, nil
	}
}

// sweepWorktreeHolders removes holders whose owner is gone and whose volume
// has no running consumer.
func (r *WorktreeReconciler) sweepWorktreeHolders(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) (bool, error) {
	state, err := worktreeownership.Decode(worktree)
	if err != nil || len(state.Holders) == 0 {
		return false, err
	}
	inUse, checked := false, false
	store := (worktreeownership.MountAccess{Client: r.Client, Reader: r.APIReader}).Store(client.ObjectKeyFromObject(worktree), worktree.UID)
	removed, err := holdset.Sweep(ctx, store, func(holder holdset.Holder) (bool, error) {
		gone, err := holderOwnerGone(ctx, r.APIReader, worktree.Namespace, holder)
		if err != nil || !gone {
			return false, err
		}
		if !checked {
			if inUse, err = claimInUse(ctx, r.APIReader, worktree.Namespace, worktree.Status.VolumeClaimName); err != nil {
				return false, err
			}
			checked = true
		}
		return !inUse, nil
	})
	if err != nil {
		return false, fmt.Errorf("sweep Worktree holders: %w", err)
	}
	for _, holder := range removed {
		logf.FromContext(ctx).Info("Removed stale Worktree holder", "worktree", worktree.Name, "holder", holder.Kind+"/"+holder.Name, "mode", holder.Mode)
	}
	return len(removed) > 0, nil
}

// sweepRepositoryHolders removes Repository reservations whose owner is gone,
// only while no Pod uses the parent volume and no pending clone reads it. A
// kept stale holder makes the Repository busy, which requeues this sweep.
func (r *RepositoryReconciler) sweepRepositoryHolders(ctx context.Context, repository *repositoriesv1alpha1.Repository) error {
	gate := repositoryaccess.Gate{Client: r.Client, Reader: r.APIReader}
	stopped, checked := false, false
	removed, err := holdset.Sweep(ctx, gate.Store(repository), func(holder holdset.Holder) (bool, error) {
		gone, err := holderOwnerGone(ctx, r.APIReader, repository.Namespace, holder)
		if err != nil || !gone {
			return false, err
		}
		if !checked {
			if stopped, err = gate.ConsumersStopped(ctx, repository); err != nil {
				return false, err
			}
			checked = true
		}
		return stopped, nil
	})
	if err != nil {
		return fmt.Errorf("sweep Repository holders: %w", err)
	}
	for _, holder := range removed {
		logf.FromContext(ctx).Info("Removed stale Repository holder", "repository", repository.Name, "holder", holder.Kind+"/"+holder.Name, "mode", holder.Mode)
	}
	return nil
}
