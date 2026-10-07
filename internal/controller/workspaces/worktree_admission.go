package workspaces

import (
	"context"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

// admitWorktreeMounts reserves every read-only and writable consumer before
// runtime creation. Partial/ambiguous successes remain protected until normal
// cleanup verifies the runtime and hot-mount helpers are gone.
func (r *WorkspaceReconciler) admitWorktreeMounts(ctx context.Context, workspace *workspacesv1alpha1.Workspace, worktrees []*repositoriesv1alpha1.Worktree) (bool, error) {
	gate := worktreeownership.MountAccess{Client: r.Client, Reader: r.APIReader}
	for _, worktree := range worktrees {
		admitted, err := gate.Admit(ctx, worktree, workspace)
		if err != nil || !admitted {
			return false, err
		}
	}
	return true, nil
}

// hotMountsGone verifies teardown outside the cache before releasing admission.
func (r *WorkspaceReconciler) hotMountsGone(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (bool, error) {
	helpers := new(corev1.PodList)
	if err := r.APIReader.List(ctx, helpers, client.InNamespace(workspace.Namespace), client.MatchingLabels{workspaceManagedByLabel: workspace.Name, hotMountHelperLabel: hotMountLabelValue}); err != nil {
		return false, err
	}
	return len(helpers.Items) == 0, nil
}

// releaseWorktreeMounts discovers cleanup candidates only during topology change
// or runtime teardown. Include old mounts and ambiguous admission successes even
// when they no longer appear in Workspace spec; ReleaseExcept rechecks their UIDs.
func (r *WorkspaceReconciler) releaseWorktreeMounts(ctx context.Context, workspace *workspacesv1alpha1.Workspace, keep map[string]bool) error {
	candidates := new(repositoriesv1alpha1.WorktreeList)
	if err := r.APIReader.List(ctx, candidates, client.InNamespace(workspace.Namespace)); err != nil {
		return err
	}
	return (worktreeownership.MountAccess{Client: r.Client, Reader: r.APIReader}).ReleaseExcept(ctx, workspace, candidates.Items, keep)
}

// releaseOldWorktreeMounts checkpoints the successfully reconciled mount
// generation on the runtime Pod. Generation survives intermediate status updates
// and changes again if a partially admitted topology is reverted. Only acknowledge
// it after cleanup succeeds, so failures/restarts retry without scanning on Ready.
func (r *WorkspaceReconciler) releaseOldWorktreeMounts(ctx context.Context, workspace *workspacesv1alpha1.Workspace, pod *corev1.Pod, worktrees []*repositoriesv1alpha1.Worktree) error {
	const annotation = "workspaces.rc.ayaka.io/worktree-mount-generation"
	generation := strconv.FormatInt(workspace.Generation, 10)
	if pod.Annotations[annotation] == generation {
		return nil
	}
	keep := make(map[string]bool, len(worktrees))
	for _, worktree := range worktrees {
		keep[worktree.Name] = true
	}
	if err := r.releaseWorktreeMounts(ctx, workspace, keep); err != nil {
		return err
	}
	before := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[annotation] = generation
	return r.Patch(ctx, pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
