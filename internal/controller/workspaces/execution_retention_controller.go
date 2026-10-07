package workspaces

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/executionretention"
)

const (
	executionTargetIndex       = "spec.targetRef.name"
	executionPodIndex          = "status.runtimePodName"
	executionCleanupBatch      = 20
	executionRetentionInterval = time.Minute
)

func executionTargetNames(object client.Object) []string {
	return []string{object.(*workspacesv1alpha1.WorkspaceExec).Spec.TargetRef.Name}
}

func executionPodNames(object client.Object) []string {
	name := object.(*workspacesv1alpha1.WorkspaceExec).Status.RuntimePodName
	if name == "" {
		return nil
	}
	return []string{name}
}

// reconcileExecutionHistory prunes history under policy. A nil policy keeps
// all history and performs no reads.
func (r *executionRetentionService) reconcileExecutionHistory(ctx context.Context, namespace string, target workspacesv1alpha1.WorkspaceExecTargetReference, targetUID types.UID, policy *workspacesv1alpha1.ExecutionRetentionPolicy) error {
	if policy == nil {
		return nil
	}
	_, err := r.pruneExecutionHistory(ctx, namespace, target, targetUID, policy)
	return err
}

// executionHistorySummary counts one target's records from the planning
// snapshot. Records owned by an earlier same-name target are not counted.
type executionHistorySummary struct {
	retained int32
	pending  int32
}

// pruneExecutionHistory uses a target-scoped snapshot only to plan work.
// Every mutation re-reads the execution and policy directly from the API. One
// item's conflict is deferred to the next pass rather than starving its peers.
// The summary is nil only when the snapshot could not be listed.
func (r *executionRetentionService) pruneExecutionHistory(ctx context.Context, namespace string, target workspacesv1alpha1.WorkspaceExecTargetReference, targetUID types.UID, policy *workspacesv1alpha1.ExecutionRetentionPolicy) (*executionHistorySummary, error) {
	executions, err := r.executionHistory(ctx, namespace, target)
	if err != nil {
		return nil, err
	}
	owned := func(p *workspacesv1alpha1.WorkspaceExec) bool {
		owner := metav1.GetControllerOf(p)
		return owner == nil || targetUID == "" || owner.UID == targetUID
	}
	summary := &executionHistorySummary{}
	deleting := 0
	for i := range executions {
		p := &executions[i]
		if !owned(p) {
			continue
		}
		summary.retained++
		if !p.DeletionTimestamp.IsZero() {
			deleting++
		}
	}
	plan := executionretention.Build(executions, policy, time.Now())
	expired := 0
	for _, i := range plan.Remove {
		if owned(&executions[i]) {
			expired++
		}
	}
	summary.pending = int32(deleting + expired)
	summary.retained -= summary.pending
	budget := executionCleanupBatch - deleting
	var failures []error
	for _, i := range plan.Remove {
		if budget <= 0 {
			break
		}
		selected, err := r.requestExecutionCleanup(ctx, &executions[i])
		// An ambiguous write response still consumes a slot until the next list.
		if selected {
			budget--
		}
		if err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("request cleanup for %s: %w", executions[i].Name, err))
		}
	}
	return summary, errors.Join(failures...)
}

func (r *executionRetentionService) executionHistory(ctx context.Context, namespace string, target workspacesv1alpha1.WorkspaceExecTargetReference) ([]workspacesv1alpha1.WorkspaceExec, error) {
	listed := new(workspacesv1alpha1.WorkspaceExecList)
	if err := r.APIReader.List(ctx, listed, client.InNamespace(namespace), client.MatchingFields{executionTargetIndex: target.Name}); err != nil {
		return nil, fmt.Errorf("list execution history: %w", err)
	}
	executions := make([]workspacesv1alpha1.WorkspaceExec, 0, len(listed.Items))
	for _, p := range listed.Items {
		if p.Spec.TargetRef == target {
			executions = append(executions, p)
		}
	}
	return executions, nil
}

// currentExecutionPolicy reads the target separately: changing policy does not
// bump any execution's resourceVersion. A deleting or temporary target retains
// its existing whole-target deletion lifecycle.
func (r *executionRetentionService) currentExecutionPolicy(ctx context.Context, process *workspacesv1alpha1.WorkspaceExec) (*workspacesv1alpha1.ExecutionRetentionPolicy, error) {
	target, err := readExecutionTarget(ctx, r.APIReader, process)
	if err != nil || target == nil {
		return nil, err
	}
	policy, _ := executionretention.PolicyFor(target)
	return policy, nil
}

// executionCleanupCandidate validates an API-fresh object, including identity,
// phase, pin, completion time and policy. Count-only eviction additionally
// re-plans the current target list, because changed peers affect the rank.
func (r *executionRetentionService) executionCleanupCandidate(ctx context.Context, snapshot *workspacesv1alpha1.WorkspaceExec) (*workspacesv1alpha1.WorkspaceExec, error) {
	current := new(workspacesv1alpha1.WorkspaceExec)
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot), current); err != nil {
		return nil, err
	}
	if current.UID != snapshot.UID || current.Spec.TargetRef != snapshot.Spec.TargetRef || !current.DeletionTimestamp.IsZero() || current.Spec.Retain || !current.Status.Phase.Terminal() || current.Status.CompletedAt == nil {
		return nil, nil
	}
	policy, err := r.currentExecutionPolicy(ctx, current)
	if err != nil || policy == nil {
		return nil, err
	}
	plan := executionretention.Build([]workspacesv1alpha1.WorkspaceExec{*current}, policy, time.Now())
	if len(plan.Remove) != 0 {
		return current, nil
	}
	executions, err := r.executionHistory(ctx, current.Namespace, current.Spec.TargetRef)
	if err != nil {
		return nil, err
	}
	// Old dependents of a same-name target must not consume this owner's bound.
	if owner := metav1.GetControllerOf(current); owner != nil {
		executions = slices.DeleteFunc(executions, func(peer workspacesv1alpha1.WorkspaceExec) bool {
			peerOwner := metav1.GetControllerOf(&peer)
			return peerOwner != nil && peerOwner.UID != owner.UID
		})
	}
	// The individual Get is authoritative for the object we will mutate. An API
	// change after it is rejected by Update/Delete's resourceVersion precondition.
	index := slices.IndexFunc(executions, func(p workspacesv1alpha1.WorkspaceExec) bool { return p.UID == current.UID && p.Name == current.Name })
	if index < 0 {
		return nil, nil
	}
	executions[index] = *current
	plan = executionretention.Build(executions, policy, time.Now())
	if slices.Contains(plan.Remove, index) {
		return current, nil
	}
	return nil, nil
}

func (r *executionRetentionService) requestExecutionCleanup(ctx context.Context, snapshot *workspacesv1alpha1.WorkspaceExec) (bool, error) {
	current, err := r.executionCleanupCandidate(ctx, snapshot)
	if err != nil || current == nil {
		return false, err
	}
	// Protect legacy terminal records before deletion. Revalidate after this
	// write as a user may have pinned the execution or disabled retention meanwhile.
	if !controllerutil.ContainsFinalizer(current, executionFinalizer) {
		controllerutil.AddFinalizer(current, executionFinalizer)
		if err := r.Update(ctx, current); err != nil {
			return false, err
		}
		current, err = r.executionCleanupCandidate(ctx, snapshot)
		if err != nil || current == nil {
			return false, err
		}
	}
	uid, version := current.UID, current.ResourceVersion
	if err := r.Delete(ctx, current, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}); err != nil {
		return true, err
	}
	logf.FromContext(ctx).Info("Requested expired WorkspaceExec deletion", "name", current.Name, "target", current.Spec.TargetRef.Name)
	return true, nil
}
