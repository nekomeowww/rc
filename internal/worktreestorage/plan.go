// Package worktreestorage plans child PVC storage from observed source state.
// It has no Kubernetes client dependency; controllers and CLI preflight checks
// must use the same rules rather than guessing CSI capabilities from class names.
package worktreestorage

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

const (
	// SourceNotReady means planning must wait for observable source storage.
	SourceNotReady = "CloneSourceNotReady"
	// SizeTooSmall means an explicit size cannot contain the source volume.
	SizeTooSmall = "CloneSizeTooSmall"
	// StorageInvalid means the requested storage cannot host a Worktree.
	StorageInvalid = "CloneStorageInvalid"
)

// Plan describes the storage of one new clone. It owns its quantities and slices.
type Plan struct {
	StorageClassName string
	Size             resource.Quantity
	AccessModes      []corev1.PersistentVolumeAccessMode
	VolumeMode       corev1.PersistentVolumeMode
}

// PlanError describes a rejected plan, suitable for a Worktree Ready Condition.
type PlanError struct {
	// Reason is a stable Condition reason shared with CLI waiters.
	Reason string
	// Message explains the observed state and the invalid or missing input.
	Message string
}

// Error returns the diagnostic suitable for a user-facing Condition message.
func (e *PlanError) Error() string { return e.Message }

// Retryable reports whether new source observations may complete this plan.
func (e *PlanError) Retryable() bool { return e.Reason == SourceNotReady }

// IsTerminalReason lets clients stop waiting when controller planning rejects
// immutable Worktree input, without duplicating the planner's error taxonomy.
func IsTerminalReason(reason string) bool {
	return reason == SizeTooSmall || reason == StorageInvalid
}

// PlanClone validates a new Worktree clone against the actual source PVC.
// Defaults inherit the source class and access modes. Repository size is only a
// desired default; max(source request, source capacity) is the required minimum.
// Explicit overrides below that minimum are rejected rather than silently allocating
// a larger volume. Missing source status is retryable, never guessed.
//
// This is a creation-time plan, not a reconciliation target for an existing child:
// a completed clone is independent of subsequent parent changes. Cross-class and
// explicit access-mode overrides remain subject to CSI support; Kubernetes does
// not publish a per-StorageClass clone/access-mode capability matrix.
func PlanClone(source *corev1.PersistentVolumeClaim, repository *repositoriesv1alpha1.Repository, override *repositoriesv1alpha1.WorktreeStorageSpec) (Plan, *PlanError) {
	if source == nil || source.Status.Phase != corev1.ClaimBound || !source.DeletionTimestamp.IsZero() {
		return Plan{}, &PlanError{Reason: SourceNotReady, Message: "Source PVC must exist, be Bound, and not be deleting before cloning"}
	}
	request := source.Spec.Resources.Requests[corev1.ResourceStorage]
	capacity := source.Status.Capacity[corev1.ResourceStorage]
	if request.Sign() <= 0 || capacity.Sign() <= 0 {
		return Plan{}, &PlanError{Reason: SourceNotReady, Message: "Source PVC must report a positive storage request and status capacity before cloning"}
	}
	if source.Spec.StorageClassName == nil || *source.Spec.StorageClassName == "" {
		return Plan{}, &PlanError{Reason: StorageInvalid, Message: "Source PVC must have a StorageClass for dynamic CSI cloning"}
	}
	// rc checks out Git via volumeMounts; a raw block clone cannot host that path.
	// Nil volumeMode has the Kubernetes Filesystem default.
	if source.Spec.VolumeMode != nil && *source.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return Plan{}, &PlanError{Reason: StorageInvalid, Message: "Worktree requires a Filesystem source PVC; block volumes cannot host a Git checkout"}
	}

	// The provisioner checks request, while the CSI volume may already be larger
	// (rounding or expansion). Neither a stale Repository size nor either smaller
	// source observation may lower this floor.
	// https://github.com/kubernetes-csi/external-provisioner/blob/master/pkg/controller/controller.go
	minimum := request.DeepCopy()
	if capacity.Cmp(minimum) > 0 {
		minimum = capacity.DeepCopy()
	}
	plan := Plan{
		StorageClassName: *source.Spec.StorageClassName,
		Size:             minimum.DeepCopy(),
		AccessModes:      slices.Clone(source.Spec.AccessModes),
		VolumeMode:       corev1.PersistentVolumeFilesystem,
	}
	if repository.Spec.Storage.Size.Cmp(plan.Size) > 0 {
		plan.Size = repository.Spec.Storage.Size.DeepCopy()
	}
	if override != nil {
		if override.StorageClassName != "" {
			plan.StorageClassName = override.StorageClassName
		}
		if override.Size != nil {
			if override.Size.Cmp(minimum) < 0 {
				return Plan{}, &PlanError{Reason: SizeTooSmall, Message: fmt.Sprintf("Requested clone size %s is smaller than source minimum %s (request %s, capacity %s); recreate the Worktree with size at least %s or omit size", override.Size.String(), minimum.String(), request.String(), capacity.String(), minimum.String())}
			}
			plan.Size = override.Size.DeepCopy()
		}
		if len(override.AccessModes) > 0 {
			plan.AccessModes = slices.Clone(override.AccessModes)
		}
	}
	if err := validateAccessModes(plan.AccessModes); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// validateAccessModes checks Kubernetes syntax and the writable Git contract,
// without claiming knowledge of any particular driver's supported modes.
func validateAccessModes(modes []corev1.PersistentVolumeAccessMode) *PlanError {
	writable := false
	for _, mode := range modes {
		switch mode {
		case corev1.ReadWriteOnce, corev1.ReadWriteMany, corev1.ReadWriteOncePod:
			writable = true
		case corev1.ReadOnlyMany:
		default:
			return &PlanError{Reason: StorageInvalid, Message: fmt.Sprintf("Unsupported PVC access mode %q", mode)}
		}
	}
	if !writable {
		return &PlanError{Reason: StorageInvalid, Message: "Worktree requires a writable access mode to initialize its Git checkout"}
	}
	if slices.Contains(modes, corev1.ReadWriteOncePod) && len(modes) > 1 {
		return &PlanError{Reason: StorageInvalid, Message: "ReadWriteOncePod cannot be combined with other PVC access modes"}
	}
	return nil
}
