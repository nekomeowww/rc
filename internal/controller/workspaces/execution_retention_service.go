package workspaces

import (
	"context"
	"errors"
	"fmt"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/executionretention"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
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
	// APIReader is required. Callers pass their own uncached reader.
	APIReader client.Reader
	Runtime   processruntime.TranscriptPruner
}

// reconcileTarget prunes a live target's execution history under its policy,
// progresses transcript cleanup, and publishes status.executionHistory. A
// deleting target is left to its whole-target lifecycle. A temporary Workspace
// has no history-pruning policy but still discharges explicit transcript
// deletions; it publishes no history status.
func (r *executionRetentionService) reconcileTarget(ctx context.Context, target client.Object) (ctrl.Result, error) {
	if !target.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	policy, ok := executionretention.PolicyFor(target)
	var summary *executionHistorySummary
	var historyErr error
	if ok {
		summary, historyErr = r.pruneExecutionHistory(ctx, target.GetNamespace(), executionTargetReference(target), target.GetUID(), policy)
	}
	var statusErr error
	if !ok || summary != nil {
		statusErr = r.publishExecutionHistory(ctx, target, policy, summary)
	}
	cleanupErr := r.reconcileTranscripts(ctx, target, policy)
	// Explicit deletion obligations must progress even after policy is disabled.
	return ctrl.Result{RequeueAfter: executionRetentionInterval}, errors.Join(historyErr, statusErr, cleanupErr)
}

// publishExecutionHistory writes the summary and ExecutionHistoryCompliant
// condition only when they change. A nil summary removes both. The optimistic
// lock rejects a stale target; the conflict is returned and retried.
func (r *executionRetentionService) publishExecutionHistory(ctx context.Context, target client.Object, policy *workspacesv1alpha1.ExecutionRetentionPolicy, summary *executionHistorySummary) error {
	var history **workspacesv1alpha1.ExecutionHistoryStatus
	var conditions *[]metav1.Condition
	before := target.DeepCopyObject().(client.Object)
	switch target := target.(type) {
	case *workspacesv1alpha1.Workspace:
		history, conditions = &target.Status.ExecutionHistory, &target.Status.Conditions
	case *workspacesv1alpha1.WorkspaceEnvironment:
		history, conditions = &target.Status.ExecutionHistory, &target.Status.Conditions
	default:
		return nil
	}
	if summary == nil {
		*history = nil
		meta.RemoveStatusCondition(conditions, workspacesv1alpha1.ConditionExecutionHistoryCompliant)
	} else {
		*history, *conditions = executionHistoryStatus(target.GetGeneration(), policy, summary, *conditions)
	}
	if apiequality.Semantic.DeepEqual(before, target) {
		return nil
	}
	if err := r.Status().Patch(ctx, target, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("update execution history status: %w", err)
	}
	return nil
}

// executionHistoryStatus is pure so the published values can be tested
// without a client. conditions is modified in place and returned.
func executionHistoryStatus(generation int64, policy *workspacesv1alpha1.ExecutionRetentionPolicy, summary *executionHistorySummary, conditions []metav1.Condition) (*workspacesv1alpha1.ExecutionHistoryStatus, []metav1.Condition) {
	ttl, maxEntries := executionretention.Effective(policy)
	status := &workspacesv1alpha1.ExecutionHistoryStatus{
		Retained: summary.retained, PendingCleanup: summary.pending,
		EffectiveMaxEntries: maxEntries, ObservedGeneration: generation,
	}
	if policy != nil {
		status.EffectiveTTL = &metav1.Duration{Duration: ttl}
	}
	condition := metav1.Condition{
		Type: workspacesv1alpha1.ConditionExecutionHistoryCompliant, Status: metav1.ConditionTrue, ObservedGeneration: generation,
		Reason: workspacesv1alpha1.ReasonWithinPolicy, Message: "No WorkspaceExec records wait for cleanup",
	}
	switch {
	case summary.pending > 0:
		condition.Status, condition.Reason = metav1.ConditionFalse, workspacesv1alpha1.ReasonCleanupBacklog
		condition.Message = fmt.Sprintf("%d WorkspaceExec records wait for cleanup", summary.pending)
	case policy == nil:
		condition.Reason = workspacesv1alpha1.ReasonPolicyUnset
		condition.Message = "No executionRetention policy; history is kept until deleted explicitly"
	}
	meta.SetStatusCondition(&conditions, condition)
	return status, conditions
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
