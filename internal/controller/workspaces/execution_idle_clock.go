package workspaces

import (
	"context"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// preserveTargetIdleClock separates runtime idleness from removable history.
// Commit the monotonic target timestamp before dropping the execution finalizer.
func (r *WorkspaceExecReconciler) preserveTargetIdleClock(ctx context.Context, process *workspacesv1alpha1.WorkspaceExec) error {
	if process.Status.CompletedAt == nil {
		return nil
	}
	target, err := readExecutionTarget(ctx, r.APIReader, process)
	if err != nil || target == nil {
		return err
	}
	if !target.GetDeletionTimestamp().IsZero() {
		return nil
	}
	var last **metav1.Time
	switch target := target.(type) {
	case *workspacesv1alpha1.Workspace:
		last = &target.Status.LastExecutionCompletedAt
	case *workspacesv1alpha1.WorkspaceEnvironment:
		last = &target.Status.LastExecutionCompletedAt
	default:
		return nil
	}
	if *last != nil && !process.Status.CompletedAt.After((*last).Time) {
		return nil
	}
	*last = process.Status.CompletedAt.DeepCopy()
	return r.Status().Update(ctx, target)
}
