package worktreeownership

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

// Before the hold set, every Worktree writer and the deletion protocol created
// one Lease per Worktree. New controllers and rcctl never create these Leases;
// they read them for one release so an upgraded cluster keeps every writer
// that an older controller or CLI admitted.
const (
	// LegacyHolderLabel names the resource that held a legacy write Lease.
	LegacyHolderLabel  = "workspaces.rc.ayaka.io/write-holder"
	legacyLeasePrefix  = "rc-worktree-"
	legacyDeletePrefix = "worktree-delete/"
)

// LegacyWriteLeaseName returns the name of a Worktree's legacy write Lease.
func LegacyWriteLeaseName(worktree *repositoriesv1alpha1.Worktree) string {
	identity := string(worktree.UID)
	if identity == "" {
		identity = worktree.Namespace + "/" + worktree.Name
	}
	sum := sha256.Sum256([]byte(identity))
	return legacyLeasePrefix + hex.EncodeToString(sum[:10])
}

// LegacyDeletionHolder is the holder identity of a legacy deletion Lease.
func LegacyDeletionHolder(worktree *repositoriesv1alpha1.Worktree) string {
	return legacyDeletePrefix + string(worktree.UID)
}

// LegacyWriter describes a live legacy write Lease.
type LegacyWriter struct {
	Lease *coordinationv1.Lease
	// Holder is the UID in holderIdentity; empty for a deletion holder.
	Holder types.UID
	// Deletion marks the old deletion protocol's own Lease.
	Deletion bool
	// Name is the holder's name for messages.
	Name string
}

// LegacyWriterOf reads a Worktree's legacy write Lease outside the cache.
// It returns nil when none exists. A Lease being garbage collected still
// counts: only its disappearance proves that its holder has stopped.
func LegacyWriterOf(ctx context.Context, reader client.Reader, worktree *repositoriesv1alpha1.Worktree) (*LegacyWriter, error) {
	lease := new(coordinationv1.Lease)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: worktree.Namespace, Name: LegacyWriteLeaseName(worktree)}, lease); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	writer := &LegacyWriter{Lease: lease, Name: lease.Labels[LegacyHolderLabel]}
	if lease.Spec.HolderIdentity != nil {
		identity := *lease.Spec.HolderIdentity
		if strings.HasPrefix(identity, legacyDeletePrefix) {
			writer.Deletion = true
		} else {
			writer.Holder = types.UID(identity)
		}
		if writer.Name == "" {
			writer.Name = identity
		}
	}
	return writer, nil
}

// ReleaseLegacyWriteLeases deletes legacy write Leases that an older
// controller or CLI created for holder, except those named in keep. Call it
// where the holder would have released its Lease: after every consumer it
// created on those Worktrees has stopped. A nil keep releases all.
func ReleaseLegacyWriteLeases(ctx context.Context, kube client.Client, reader client.Reader, namespace, holderName string, holderUID types.UID, keep map[string]bool) error {
	leases := new(coordinationv1.LeaseList)
	if err := reader.List(ctx, leases, client.InNamespace(namespace), client.MatchingLabels{LegacyHolderLabel: holderName}); err != nil {
		return fmt.Errorf("list legacy Worktree write Leases: %w", err)
	}
	for index := range leases.Items {
		lease := &leases.Items[index]
		if keep[lease.Name] || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(holderUID) {
			continue
		}
		if err := kube.Delete(ctx, lease, client.Preconditions{UID: &lease.UID}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("release legacy Worktree write Lease: %w", err)
		}
	}
	return nil
}
