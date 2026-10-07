package worktreeownership

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	mountHoldersAnnotation = "repositories.rc.ayaka.io/mount-holders"
	mountsClosedAnnotation = "repositories.rc.ayaka.io/mounts-closed"
)

// MountAccess serializes runtime admission and deletion on the Worktree itself.
// Both sides use resourceVersion preconditions on this same object. A Lease can
// disappear during foreground GC; this fence lives until the Worktree is gone.
// Workspace spec is a desired mount, not permission to create a consumer.
// Reservations have no TTL: time passing cannot stop an admitted runtime.
type MountAccess struct {
	Client client.Client
	// Reader is required and must bypass the manager cache in production.
	Reader client.Reader
}

// MountsClosed reports an irreversible storage/deletion fence. A Worktree whose
// storage was explicitly deleted must not silently become a fresh checkout.
func MountsClosed(worktree *repositoriesv1alpha1.Worktree) bool {
	return !worktree.DeletionTimestamp.IsZero() || worktree.Annotations[mountsClosedAnnotation] != ""
}

func mountToken(workspace *workspacesv1alpha1.Workspace) string {
	return string(workspace.UID) + "/" + workspace.Name
}

func mountHolders(worktree *repositoriesv1alpha1.Worktree) (map[string]bool, error) {
	holders := make(map[string]bool)
	if data := worktree.Annotations[mountHoldersAnnotation]; data != "" {
		if err := json.Unmarshal([]byte(data), &holders); err != nil {
			return nil, fmt.Errorf("decode Worktree mount holders: %w", err)
		}
	}
	if holders == nil {
		holders = make(map[string]bool)
	}
	return holders, nil
}

func storeMountHolders(worktree *repositoriesv1alpha1.Worktree, holders map[string]bool) error {
	encoded, err := json.Marshal(holders)
	if err != nil {
		return err
	}
	if worktree.Annotations == nil {
		worktree.Annotations = make(map[string]string)
	}
	worktree.Annotations[mountHoldersAnnotation] = string(encoded)
	return nil
}

// Admit records an in-flight mount before any runtime or hot-mount Pod creation.
// The captured UID prevents name reuse; a DELETE or Close racing this patch
// changes resourceVersion, so a retry observes the fence and refuses admission.
// On error the write may have succeeded: retain and later release the token
// only after confirming that this Workspace's consumers have stopped.
func (g MountAccess) Admit(ctx context.Context, captured *repositoriesv1alpha1.Worktree, workspace *workspacesv1alpha1.Workspace) (bool, error) {
	owner := new(workspacesv1alpha1.Workspace)
	if err := g.Reader.Get(ctx, client.ObjectKeyFromObject(workspace), owner); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if owner.UID != workspace.UID || !owner.DeletionTimestamp.IsZero() {
		return false, nil
	}
	admitted := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		admitted = false
		current := new(repositoriesv1alpha1.Worktree)
		if err := g.Reader.Get(ctx, client.ObjectKeyFromObject(captured), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != captured.UID || current.Generation != captured.Generation || current.Status.VolumeClaimName != captured.Status.VolumeClaimName || MountsClosed(current) {
			return nil
		}
		holders, err := mountHolders(current)
		if err != nil {
			return err
		}
		if holders[mountToken(workspace)] {
			admitted = true
			return nil
		}
		before := current.DeepCopy()
		holders[mountToken(workspace)] = true
		if err := storeMountHolders(current, holders); err != nil {
			return err
		}
		if err := g.Client.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		admitted = true
		return nil
	})
	return admitted, err
}

// Close closes admission permanently and reports whether in-flight/runtime
// reservations remain. Even with no visible Pod yet, an admitted creator keeps
// deletion waiting. New spec references after this point cannot obtain access.
func (g MountAccess) Close(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) (bool, error) {
	drained := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Worktree)
		if err := g.Reader.Get(ctx, client.ObjectKeyFromObject(worktree), current); err != nil {
			return err
		}
		if current.UID != worktree.UID {
			return fmt.Errorf("worktree identity changed while closing mount admission")
		}
		holders, err := mountHolders(current)
		if err != nil {
			return err
		}
		if current.Annotations[mountsClosedAnnotation] == "" {
			before := current.DeepCopy()
			if current.Annotations == nil {
				current.Annotations = make(map[string]string)
			}
			current.Annotations[mountsClosedAnnotation] = "true"
			if err := g.Client.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
		drained = len(holders) == 0
		return nil
	})
	return drained, err
}

// ReleaseExcept drops this Workspace incarnation's unused reservations. Call
// only after removed mounts' Pods have disappeared. Candidates are captured by
// the caller during topology change or teardown, never discovered on the Ready
// path. keep contains names still used by the current topology; nil releases all.
func (g MountAccess) ReleaseExcept(ctx context.Context, workspace *workspacesv1alpha1.Workspace, candidates []repositoriesv1alpha1.Worktree, keep map[string]bool) error {
	for _, worktree := range candidates {
		if worktree.Namespace != workspace.Namespace || keep[worktree.Name] {
			continue
		}
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			current := new(repositoriesv1alpha1.Worktree)
			if err := g.Reader.Get(ctx, client.ObjectKeyFromObject(&worktree), current); err != nil {
				return client.IgnoreNotFound(err)
			}
			if current.UID != worktree.UID {
				return nil
			}
			holders, err := mountHolders(current)
			if err != nil {
				return err
			}
			if !holders[mountToken(workspace)] {
				return nil
			}
			before := current.DeepCopy()
			delete(holders, mountToken(workspace))
			if err := storeMountHolders(current, holders); err != nil {
				return err
			}
			err = g.Client.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}
