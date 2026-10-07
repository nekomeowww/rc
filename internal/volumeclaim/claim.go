// Package volumeclaim separates rc resource identities from their PVC names.
// Controllers and clients share this policy so a CLI preflight checks exactly
// the name that the controller will allocate.
package volumeclaim

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Role identifies an independently owned persistent volume within an rc kind.
type Role string

const (
	// Repository stores a Repository's parent checkout.
	Repository Role = "repository"
	// Worktree stores an independent cloned checkout.
	Worktree Role = "worktree"
	// WorkspaceHome stores a Workspace's writable home.
	WorkspaceHome Role = "workspace"
	// EnvironmentCurrent stores an Environment's initial committed home.
	EnvironmentCurrent Role = "environment-current"
	// EnvironmentDraft stores a mutable home for the next Environment revision.
	EnvironmentDraft Role = "environment-draft"
)

// Name returns a predictable DNS subdomain for a new PVC. Revision is used only
// for Environment roles. Existing consumers must read their resource's status.
// Long names retain a SHA-256 suffix over the complete untruncated name, using
// the same stable hashing family as rc's resource and Lease naming policies.
func Name(role Role, name string, revision int64) string {
	var full string
	switch role {
	case EnvironmentCurrent:
		full = fmt.Sprintf("environment-%s-current-%d", name, revision)
	case EnvironmentDraft:
		full = fmt.Sprintf("environment-%s-draft-%d", name, revision)
	case WorkspaceHome:
		full = "workspace-" + name + "-home"
	default:
		full = string(role) + "-" + name
	}
	// PVC metadata names allow 253 bytes. Hash the full name before shortening
	// so equal prefixes and different roles/revisions do not collapse together.
	if len(full) <= 253 {
		return full
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(full)))[:16]
	return strings.TrimRight(full[:253-len(digest)-1], "-.") + "-" + digest
}

func legacyName(role Role, name string, revision int64) string {
	switch role {
	case EnvironmentCurrent:
		return fmt.Sprintf("%s-current-%d", name, revision)
	case EnvironmentDraft:
		return fmt.Sprintf("%s-draft-%d", name, revision)
	default:
		return name
	}
}

// ConflictError reports the actual PVC owner without permitting adoption.
// A same-name CR recreated with a new UID is a different owner.
type ConflictError struct{ message string }

func (e *ConflictError) Error() string { return e.message }

// IsConflict distinguishes a permanent ownership conflict from an API failure.
func IsConflict(err error) bool {
	var conflict *ConflictError
	return errors.As(err, &conflict)
}

// CheckOwner rejects unowned, foreign-owned, or terminating PVCs. It must run
// before mounting, promoting, changing, or deleting an existing managed claim.
func CheckOwner(claim *corev1.PersistentVolumeClaim, owner client.Object) error {
	if owner.GetUID() != "" && metav1.IsControlledBy(claim, owner) && claim.DeletionTimestamp.IsZero() {
		return nil
	}
	actual := "no controller owner"
	if reference := metav1.GetControllerOf(claim); reference != nil {
		actual = fmt.Sprintf("%s %s (UID %s)", reference.Kind, reference.Name, reference.UID)
	}
	if !claim.DeletionTimestamp.IsZero() {
		actual += "; PVC is terminating"
	}
	return &ConflictError{message: fmt.Sprintf("PVC %s/%s cannot be used by %T %s (UID %s): %s", claim.Namespace, claim.Name, owner, owner.GetName(), owner.GetUID(), actual)}
}

// Resolve pins recorded names, recovers an owned legacy PVC after an interrupted
// status update, and otherwise selects the typed name. It never adopts a PVC or
// changes a recorded identity. API errors are propagated, not treated as absence.
func Resolve(ctx context.Context, reader client.Reader, owner client.Object, role Role, revision int64, recorded string) (string, error) {
	if recorded != "" {
		return recorded, checkName(ctx, reader, owner, recorded)
	}
	legacy := new(corev1.PersistentVolumeClaim)
	err := reader.Get(ctx, client.ObjectKey{Namespace: owner.GetNamespace(), Name: legacyName(role, owner.GetName(), revision)}, legacy)
	if err == nil && owner.GetUID() != "" && metav1.IsControlledBy(legacy, owner) {
		return legacy.Name, CheckOwner(legacy, owner)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	name := Name(role, owner.GetName(), revision)
	return name, checkName(ctx, reader, owner, name)
}

func checkName(ctx context.Context, reader client.Reader, owner client.Object, name string) error {
	claim := new(corev1.PersistentVolumeClaim)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: owner.GetNamespace(), Name: name}, claim); err != nil {
		return client.IgnoreNotFound(err)
	}
	return CheckOwner(claim, owner)
}

// Preflight checks the typed PVC selected by the caller before creating a CR.
// Other CR kinds may share its name; only occupancy of its typed PVC matters.
// Callers choose the role/revision and skip resources that do not allocate PVCs.
// This is advisory: controllers recheck ownership because creation can race.
func Preflight(ctx context.Context, reader client.Reader, owner client.Object, role Role, revision int64) error {
	if err := checkName(ctx, reader, owner, Name(role, owner.GetName(), revision)); err != nil {
		return fmt.Errorf("PVC preflight for %s/%s: %w", owner.GetNamespace(), owner.GetName(), err)
	}
	return nil
}
