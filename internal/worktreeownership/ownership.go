// Package worktreeownership defines Workspace lifecycle ownership independently
// from checkout bootstrap hints and Workspace mount references.
package worktreeownership

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/conditions"
)

// GeneratedForLabel records checkout provenance and bootstrap hints. It never
// authorizes deletion or adoption: only UID-based owner references do that.
const GeneratedForLabel = "workspaces.rc.ayaka.io/generated-for"

// WorkspaceCleanupFinalizer marks runtime teardown, which finishes before GC
// waits for dependent Worktrees. Unlike foregroundDeletion it is a live blocker.
const WorkspaceCleanupFinalizer = "workspaces.rc.ayaka.io/terminate-processes"

// DeletionFinalizer keeps a Worktree present until the controller has closed
// its hold set, drained every holder, and verified no Pod uses its volume.
const DeletionFinalizer = "repositories.rc.ayaka.io/worktree-delete-protection"

// VolumeProtectionFinalizer keeps foreground GC from removing storage before
// the Worktree's mount, runtime, writer, and clone checks have completed.
const VolumeProtectionFinalizer = "repositories.rc.ayaka.io/worktree-volume-protection"

// InitializeGenerated sets the lifecycle of a new, unpersisted Worktree. The
// Workspace must already exist so its API-assigned UID is included at creation.
// Protection is installed before GC can observe the resource, including when
// the Workspace is concurrently deleted before the first controller reconcile.
func InitializeGenerated(workspace *workspacesv1alpha1.Workspace, worktree *repositoriesv1alpha1.Worktree) {
	worktree.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(workspace, workspacesv1alpha1.SchemeGroupVersion.WithKind("Workspace"))}
	if worktree.Labels == nil {
		worktree.Labels = make(map[string]string)
	}
	worktree.Labels[GeneratedForLabel] = workspace.Name
	controllerutil.AddFinalizer(worktree, DeletionFinalizer)
}

// ReadyAtCurrentGeneration reports whether the Worktree's status and Ready
// condition are True and observed at or after its current generation.
func ReadyAtCurrentGeneration(worktree *repositoriesv1alpha1.Worktree) bool {
	return conditions.ReadyAtGeneration(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady, worktree.Generation, worktree.Status.ObservedGeneration)
}

// WorkspaceOwner returns a Workspace controller reference, without interpreting
// generated-for labels as ownership. API versions may change within the group.
func WorkspaceOwner(worktree *repositoriesv1alpha1.Worktree) *metav1.OwnerReference {
	owner := metav1.GetControllerOf(worktree)
	if owner == nil || owner.Kind != "Workspace" {
		return nil
	}
	gv, err := schema.ParseGroupVersion(owner.APIVersion)
	if err != nil || gv.Group != workspacesv1alpha1.SchemeGroupVersion.Group {
		return nil
	}
	return owner
}

// IsOwnedBy checks namespace and UID identity, so reusing a Workspace name
// cannot claim resources belonging to an earlier Workspace incarnation.
func IsOwnedBy(worktree *repositoriesv1alpha1.Worktree, workspace *workspacesv1alpha1.Workspace) bool {
	owner := WorkspaceOwner(worktree)
	return owner != nil && owner.UID != "" && owner.UID == workspace.UID && owner.Name == workspace.Name && worktree.Namespace == workspace.Namespace
}

// ReferenceBlockers lists live Workspace mounts that protect a Worktree from
// deletion. Deleting Workspaces finish runtime cleanup and release their holders
// first; counting their mounts here would deadlock foreground garbage collection.
// This is a conservative reference check, not an admission lock. Callers must
// close MountAccess and wait for its holders to drain before finalizing.
// reader should be a cache with WorkspaceWorktreeIndex; a reader without the
// index, such as a direct API client, falls back to ListReferenceBlockers.
func ReferenceBlockers(ctx context.Context, reader client.Reader, namespace, name string) ([]string, error) {
	workspaces := new(workspacesv1alpha1.WorkspaceList)
	if err := reader.List(ctx, workspaces, client.InNamespace(namespace), client.MatchingFields{WorkspaceWorktreeIndex: name}); err != nil {
		return ListReferenceBlockers(ctx, reader, namespace, name)
	}
	return referenceBlockers(workspaces.Items, name), nil
}

// WorkspaceWorktreeIndex indexes Workspaces by the Worktrees their spec mounts.
const WorkspaceWorktreeIndex = "spec.mounts.worktreeRef.name"

// WorkspaceWorktreeIndexValues extracts WorkspaceWorktreeIndex values.
func WorkspaceWorktreeIndexValues(object client.Object) []string {
	workspace, ok := object.(*workspacesv1alpha1.Workspace)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(workspace.Spec.Mounts))
	for _, mount := range workspace.Spec.Mounts {
		if mount.WorktreeRef != nil && !slices.Contains(names, mount.WorktreeRef.Name) {
			names = append(names, mount.WorktreeRef.Name)
		}
	}
	return names
}

// ListReferenceBlockers is ReferenceBlockers for clients without a cache
// index: it lists every Workspace in the namespace.
func ListReferenceBlockers(ctx context.Context, reader client.Reader, namespace, name string) ([]string, error) {
	workspaces := new(workspacesv1alpha1.WorkspaceList)
	if err := reader.List(ctx, workspaces, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Workspace references for Worktree %q: %w", name, err)
	}
	return referenceBlockers(workspaces.Items, name), nil
}

func referenceBlockers(workspaces []workspacesv1alpha1.Workspace, name string) []string {
	blockers := make([]string, 0)
	for _, workspace := range workspaces {
		if !workspace.DeletionTimestamp.IsZero() && !slices.Contains(workspace.Finalizers, WorkspaceCleanupFinalizer) {
			continue
		}
		for _, mount := range workspace.Spec.Mounts {
			if mount.WorktreeRef != nil && mount.WorktreeRef.Name == name {
				blockers = append(blockers, workspace.Name)
				break
			}
		}
	}
	slices.Sort(blockers)
	return blockers
}

// ProtectVolume upgrades an existing owned PVC before an explicit adoption can
// expose it to Workspace foreground GC. Missing or terminating storage is not
// a safe adoption target. Metadata patches use resourceVersion preconditions.
func ProtectVolume(ctx context.Context, kube client.Client, worktree *repositoriesv1alpha1.Worktree) error {
	claim := new(corev1.PersistentVolumeClaim)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: worktree.Namespace, Name: worktree.Status.VolumeClaimName}, claim); err != nil {
		return fmt.Errorf("get Worktree volume before adoption: %w", err)
	}
	if !metav1.IsControlledBy(claim, worktree) || !claim.DeletionTimestamp.IsZero() {
		return fmt.Errorf("worktree volume is deleting or has a different owner")
	}
	return EnsureVolumeProtection(ctx, kube, claim)
}

// EnsureVolumeProtection protects a live PVC on upgrade. A terminating legacy
// PVC cannot acquire new finalizers; Kubernetes rejects that transition.
func EnsureVolumeProtection(ctx context.Context, kube client.Client, claim *corev1.PersistentVolumeClaim) error {
	if !claim.DeletionTimestamp.IsZero() {
		return fmt.Errorf("PVC deletion is in progress; reconcile storage deletion before protecting it")
	}
	if controllerutil.ContainsFinalizer(claim, VolumeProtectionFinalizer) {
		return nil
	}
	before := claim.DeepCopy()
	controllerutil.AddFinalizer(claim, VolumeProtectionFinalizer)
	return kube.Patch(ctx, claim, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// ReleaseVolumeProtection removes only rc's storage guard once the Worktree is
// safe to clean up. PVC protection and provisioner finalizers remain intact.
func ReleaseVolumeProtection(ctx context.Context, kube client.Client, claim *corev1.PersistentVolumeClaim) error {
	if !controllerutil.ContainsFinalizer(claim, VolumeProtectionFinalizer) {
		return nil
	}
	before := claim.DeepCopy()
	controllerutil.RemoveFinalizer(claim, VolumeProtectionFinalizer)
	return client.IgnoreNotFound(kube.Patch(ctx, claim, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
}
