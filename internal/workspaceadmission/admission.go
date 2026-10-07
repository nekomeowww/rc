// Package workspaceadmission serializes command admission and automatic
// Workspace deletion on the same Kubernetes object.
package workspaceadmission

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

var (
	// ErrClosed means the owner is deleting or no longer admits new executions.
	ErrClosed = errors.New("workspace execution admission is closed")
	// ErrOwnerChanged prevents an old execution from targeting a replacement
	// Workspace that happens to reuse its owner's name.
	ErrOwnerChanged = errors.New("workspace execution owner has changed")
)

// Gate uses Workspace status CAS, rather than a separate lock resource, so the
// final DELETE precondition also fences concurrent admission and cancellation.
// Execution objects must exist with their finalizer before Admit, and retain
// that finalizer until their runtime has stopped. No reservation expires.
type Gate struct {
	Client client.Client
	// Reader must bypass the manager cache in production.
	Reader client.Reader
}

func (g Gate) reader() client.Reader {
	if g.Reader != nil {
		return g.Reader
	}
	return g.Client
}

// Check rejects a deleting or fenced owner before a client creates resources.
// This is early feedback only; every controller Runtime.Start must also Admit.
func Check(workspace *workspaces.Workspace) error {
	if !workspace.DeletionTimestamp.IsZero() || workspace.Status.ExecutionAdmissionClosed {
		return fmt.Errorf("%w: %s/%s", ErrClosed, workspace.Namespace, workspace.Name)
	}
	return nil
}

// Admit is the command's linearization point. The execution has already been
// persisted, so a deletion that closes the gate after this CAS will see the
// nonterminal execution in its subsequent uncached list. If closure wins first,
// this update conflicts or Check rejects it. Retry through normal reconciliation.
// No caller may start the runtime when admission returns an error.
func (g Gate) Admit(ctx context.Context, expected *workspaces.Workspace, process *workspaces.WorkspaceExec) error {
	current := new(workspaces.Workspace)
	if err := g.reader().Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
		return err
	}
	if current.UID != expected.UID {
		return ErrOwnerChanged
	}
	owner := metav1.GetControllerOf(process)
	if owner != nil && (owner.UID != current.UID || owner.Kind != "Workspace" || owner.Name != current.Name || owner.APIVersion != workspaces.SchemeGroupVersion.String()) {
		return ErrOwnerChanged
	}
	if err := Check(current); err != nil {
		return err
	}
	// Keep this conditional Update even though status is unchanged: its
	// resourceVersion check orders admission against fence closure. Successful
	// no-op updates need not increment resourceVersion; the already persisted
	// execution protects the runtime in the post-closure active scan.
	if err := g.Client.Status().Update(ctx, current); err != nil {
		return fmt.Errorf("admit Workspace execution: %w", err)
	}
	return nil
}

// Close prevents further admission before the final active-execution scan.
// The caller keeps the returned resourceVersion through DELETE; refreshing it
// after that scan would lose protection against a concurrent reopening.
func (g Gate) Close(ctx context.Context, workspace *workspaces.Workspace) error {
	if !workspace.Status.ExecutionAdmissionClosed {
		before := workspace.DeepCopy()
		workspace.Status.ExecutionAdmissionClosed = true
		if err := g.Client.Status().Patch(ctx, workspace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("close Workspace execution admission: %w", err)
		}
	}
	// An older CRD can prune this field. Confirm using a fresh object so response
	// decoding cannot leave an unpersisted value from the request in memory.
	// Do not replace the closed snapshot or its DELETE precondition with this read.
	confirmed := new(workspaces.Workspace)
	if err := g.reader().Get(ctx, client.ObjectKeyFromObject(workspace), confirmed); err != nil {
		return err
	}
	if !confirmed.Status.ExecutionAdmissionClosed || confirmed.UID != workspace.UID || confirmed.ResourceVersion != workspace.ResourceVersion {
		return fmt.Errorf("workspace execution fence was not persisted or changed before confirmation; verify the installed CRD")
	}
	return nil
}

// Reopen cancels an unsuccessful deletion or recovers a persisted fence after
// controller restart. A successful DELETE leaves deletionTimestamp set, so it
// cannot reopen. Reopening itself changes resourceVersion, invalidating any
// concurrent DELETE prepared under the old closed state.
func (g Gate) Reopen(ctx context.Context, expected *workspaces.Workspace) error {
	current := new(workspaces.Workspace)
	if err := g.reader().Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if current.UID != expected.UID || !current.DeletionTimestamp.IsZero() || !current.Status.ExecutionAdmissionClosed {
		return nil
	}
	before := current.DeepCopy()
	current.Status.ExecutionAdmissionClosed = false
	if err := g.Client.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("reopen Workspace execution admission: %w", err)
	}
	return nil
}
