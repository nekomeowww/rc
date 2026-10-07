package workspaces

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/holdset"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

// reasonWorktreeInUse reports that another writer holds a Worktree this
// Workspace mounts writable. rcctl mount returns before admission, so this
// condition is where a writer conflict surfaces.
const reasonWorktreeInUse = "WorktreeInUse"

// worktreeModes returns the holder mode each mounted Worktree needs.
func worktreeModes(resolved *resolvedWorkspace) map[string]holdset.Mode {
	modes := make(map[string]holdset.Mode, len(resolved.worktreeMounts))
	for _, worktree := range resolved.worktreeMounts {
		modes[worktree.Name] = worktreeownership.Read
	}
	for _, claim := range resolved.writeClaims {
		modes[claim.worktree] = worktreeownership.Write
	}
	return modes
}

func (r *WorkspaceReconciler) worktreeAccess() worktreeownership.MountAccess {
	return worktreeownership.MountAccess{Client: r.Client, Reader: r.APIReader}
}

// admitWorktreeMounts records every read-only and writable consumer before
// runtime or hot-mount creation, in name order. Partial/ambiguous successes
// remain protected until normal cleanup verifies the runtime and hot-mount
// helpers are gone. A refusal returns a Ready reason and message.
func (r *WorkspaceReconciler) admitWorktreeMounts(ctx context.Context, workspace *workspacesv1alpha1.Workspace, worktrees []*repositoriesv1alpha1.Worktree, modes map[string]holdset.Mode) (string, string, error) {
	ordered := slices.Clone(worktrees)
	slices.SortFunc(ordered, func(left, right *repositoriesv1alpha1.Worktree) int { return strings.Compare(left.Name, right.Name) })
	for _, worktree := range ordered {
		mode := modes[worktree.Name]
		if mode == "" {
			mode = worktreeownership.Read
		}
		if mode == worktreeownership.Write {
			if reason, message, err := r.legacyWriterConflict(ctx, workspace, worktree); err != nil || reason != "" {
				return reason, message, err
			}
		}
		result, err := r.worktreeAccess().Admit(ctx, worktree, workspace, worktreeownership.WorkspaceHolder(workspace, mode))
		if err != nil {
			return "", "", err
		}
		if !result.Admitted {
			reason, message := refusal(worktree, result)
			return reason, message, nil
		}
	}
	return "", "", nil
}

// legacyWriterConflict reports a write Lease that an older controller or CLI
// created for another holder. It blocks until garbage collection removes it.
func (r *WorkspaceReconciler) legacyWriterConflict(ctx context.Context, workspace *workspacesv1alpha1.Workspace, worktree *repositoriesv1alpha1.Worktree) (string, string, error) {
	legacy, err := worktreeownership.LegacyWriterOf(ctx, r.APIReader, worktree)
	if err != nil || legacy == nil || legacy.Holder == workspace.UID {
		return "", "", err
	}
	return reasonWorktreeInUse, fmt.Sprintf("Worktree %s is held by legacy writer %q (Lease %s)", worktree.Name, legacy.Name, legacy.Lease.Name), nil
}

// refusal explains why admission refused a holder.
func refusal(worktree *repositoriesv1alpha1.Worktree, result holdset.Result) (string, string) {
	if len(result.Conflicts) == 0 {
		return reasonWorktreeDeleting, "Worktree mount admission is closed"
	}
	holders := make([]string, 0, len(result.Conflicts))
	for _, holder := range result.Conflicts {
		holders = append(holders, holder.Kind+"/"+holder.Name)
	}
	return reasonWorktreeInUse, fmt.Sprintf("Worktree %s is held for writing by %s; the writable mount starts after it is released", worktree.Name, strings.Join(holders, ", "))
}

// verifyWriteClaims confirms that this Workspace still holds every Worktree
// the running Pod writes, admitting writable mounts not yet in use. lost means
// a Worktree the runtime or a helper already uses is held by someone else;
// otherwise a refusal is a conflict to report while the runtime keeps running.
func (r *WorkspaceReconciler) verifyWriteClaims(ctx context.Context, workspace *workspacesv1alpha1.Workspace, pod *corev1.Pod, claims []workspaceWriteClaim) (bool, string, string, error) {
	names := make([]string, 0, len(claims))
	for _, claim := range claims {
		names = append(names, claim.worktree)
	}
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		worktree := new(repositoriesv1alpha1.Worktree)
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: workspace.Namespace, Name: name}, worktree); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return false, "", "", err
		}
		reason, message, err := r.legacyWriterConflict(ctx, workspace, worktree)
		if err != nil {
			return false, "", "", err
		}
		if reason == "" {
			result, err := r.worktreeAccess().Admit(ctx, worktree, workspace, worktreeownership.WorkspaceHolder(workspace, worktreeownership.Write))
			if err != nil {
				return false, "", "", err
			}
			if result.Admitted || result.Held {
				continue
			}
			reason, message = refusal(worktree, result)
		}
		inUse, err := r.workspaceUsesClaim(ctx, workspace, pod, worktree.Status.VolumeClaimName)
		if err != nil {
			return false, "", "", err
		}
		return inUse, reason, message, nil
	}
	return false, "", "", nil
}

// workspaceUsesClaim reports whether the runtime or a hot-mount helper of this
// Workspace already mounts claimName, read outside the cache.
func (r *WorkspaceReconciler) workspaceUsesClaim(ctx context.Context, workspace *workspacesv1alpha1.Workspace, pod *corev1.Pod, claimName string) (bool, error) {
	if claimName == "" {
		return false, nil
	}
	helpers := new(corev1.PodList)
	if err := r.APIReader.List(ctx, helpers, client.InNamespace(workspace.Namespace), client.MatchingLabels{workspaceManagedByLabel: workspace.Name, hotMountHelperLabel: hotMountLabelValue}); err != nil {
		return false, err
	}
	for _, candidate := range append([]corev1.Pod{*pod}, helpers.Items...) {
		for _, volume := range candidate.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claimName {
				return true, nil
			}
		}
	}
	return false, nil
}

// workspacePodWriteClaims returns the Worktrees a running Pod was admitted to
// write. Pods created before the hold set list legacy write Lease names.
func (r *WorkspaceReconciler) workspacePodWriteClaims(ctx context.Context, pod *corev1.Pod) ([]workspaceWriteClaim, error) {
	if encoded := pod.Annotations[workspaceWriteWorktreesAnnotation]; encoded != "" {
		names := make([]string, 0)
		if err := json.Unmarshal([]byte(encoded), &names); err != nil {
			return nil, fmt.Errorf("decode Workspace runtime Pod write Worktrees: %w", err)
		}
		claims := make([]workspaceWriteClaim, 0, len(names))
		for _, name := range names {
			claims = append(claims, workspaceWriteClaim{worktree: name})
		}
		return claims, nil
	}
	encoded := pod.Annotations[legacyWorkspaceWriteClaimsAnnotation]
	if encoded == "" {
		return nil, nil
	}
	leaseNames := make([]string, 0)
	if err := json.Unmarshal([]byte(encoded), &leaseNames); err != nil {
		return nil, fmt.Errorf("decode Workspace runtime Pod write claims: %w", err)
	}
	worktrees := new(repositoriesv1alpha1.WorktreeList)
	if err := r.APIReader.List(ctx, worktrees, client.InNamespace(pod.Namespace)); err != nil {
		return nil, err
	}
	claims := make([]workspaceWriteClaim, 0, len(leaseNames))
	for index := range worktrees.Items {
		if slices.Contains(leaseNames, worktreeownership.LegacyWriteLeaseName(&worktrees.Items[index])) {
			claims = append(claims, workspaceWriteClaim{worktree: worktrees.Items[index].Name})
		}
	}
	return claims, nil
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
// when they no longer appear in Workspace spec; Retain rechecks their UIDs.
// keep maps names still used by the current topology to their mode; nil
// releases all, including legacy write Leases created by older controllers.
func (r *WorkspaceReconciler) releaseWorktreeMounts(ctx context.Context, workspace *workspacesv1alpha1.Workspace, keep map[string]holdset.Mode) error {
	candidates := new(repositoriesv1alpha1.WorktreeList)
	if err := r.APIReader.List(ctx, candidates, client.InNamespace(workspace.Namespace)); err != nil {
		return err
	}
	if err := r.worktreeAccess().Retain(ctx, workspace, candidates.Items, keep); err != nil {
		return err
	}
	keepLeases := make(map[string]bool)
	for index := range candidates.Items {
		if keep[candidates.Items[index].Name] == worktreeownership.Write {
			keepLeases[worktreeownership.LegacyWriteLeaseName(&candidates.Items[index])] = true
		}
	}
	return worktreeownership.ReleaseLegacyWriteLeases(ctx, r.Client, r.APIReader, workspace.Namespace, workspace.Name, workspace.UID, keepLeases)
}

// releaseOldWorktreeMounts checkpoints the successfully reconciled mount
// generation on the runtime Pod. Generation survives intermediate status updates
// and changes again if a partially admitted topology is reverted. Only acknowledge
// it after cleanup succeeds, so failures/restarts retry without scanning on Ready.
// It releases unmounted Worktrees and downgrades writers that became read-only.
func (r *WorkspaceReconciler) releaseOldWorktreeMounts(ctx context.Context, workspace *workspacesv1alpha1.Workspace, pod *corev1.Pod, keep map[string]holdset.Mode) error {
	const annotation = "workspaces.rc.ayaka.io/worktree-mount-generation"
	generation := strconv.FormatInt(workspace.Generation, 10)
	if pod.Annotations[annotation] == generation {
		return nil
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
