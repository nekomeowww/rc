package workspaces

import (
	"context"
	"errors"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// executionRetentionService composes CR policy and storage cleanup without
// making either layer responsible for the other's lifecycle.
type executionRetentionService struct {
	client.Client
	APIReader client.Reader
	Runtime   processruntime.TranscriptPruner
}

func (r *executionRetentionService) reconcileTarget(ctx context.Context, target client.Object, policy *workspacesv1alpha1.ExecutionRetentionPolicy) (ctrl.Result, error) {
	if !target.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	result, historyErr := r.reconcileExecutionHistory(ctx, target.GetNamespace(), executionTargetReference(target), target.GetUID(), policy)
	cleanupErr := r.reconcileTranscripts(ctx, target, policy)
	// Explicit deletion obligations must progress even after policy is disabled.
	result.RequeueAfter = executionRetentionInterval
	return result, errors.Join(historyErr, cleanupErr)
}

func executionTargetReference(target client.Object) workspacesv1alpha1.WorkspaceExecTargetReference {
	kind := workspacesv1alpha1.WorkspaceExecTargetWorkspace
	if _, ok := target.(*workspacesv1alpha1.WorkspaceEnvironment); ok {
		kind = workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment
	}
	return workspacesv1alpha1.WorkspaceExecTargetReference{Kind: kind, Name: target.GetName()}
}

// readExecutionTarget rejects same-name replacements rather than borrowing
// their storage or lifecycle. A nil object means the original target is gone.
func readExecutionTarget(ctx context.Context, reader client.Reader, process *workspacesv1alpha1.WorkspaceExec) (client.Object, error) {
	var target client.Object
	switch process.Spec.TargetRef.Kind {
	case workspacesv1alpha1.WorkspaceExecTargetWorkspace:
		target = new(workspacesv1alpha1.Workspace)
	case workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment:
		target = new(workspacesv1alpha1.WorkspaceEnvironment)
	default:
		return nil, nil
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: process.Namespace, Name: process.Spec.TargetRef.Name}, target); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	owner := metav1.GetControllerOf(process)
	if owner == nil || owner.APIVersion != workspacesv1alpha1.GroupVersion.String() || owner.Kind != string(process.Spec.TargetRef.Kind) || owner.Name != process.Spec.TargetRef.Name || owner.UID == "" || target.GetUID() == "" || owner.UID != target.GetUID() {
		return nil, nil
	}
	return target, nil
}

func retentionTargetForProcess(kind workspacesv1alpha1.WorkspaceExecTargetKind) handler.MapFunc {
	return func(_ context.Context, object client.Object) []reconcile.Request {
		process, ok := object.(*workspacesv1alpha1.WorkspaceExec)
		if !ok || process.Spec.TargetRef.Kind != kind {
			return nil
		}
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: process.Namespace, Name: process.Spec.TargetRef.Name}}}
	}
}
