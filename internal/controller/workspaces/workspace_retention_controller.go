/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package workspaces

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/workspaceadmission"
)

const (
	temporaryWorkspaceCleanupDelay = 5 * time.Minute
	temporaryWorkspaceStartTimeout = 15 * time.Minute
)

// WorkspaceRetentionReconciler applies idle suspension and deletes Workspaces
// whose retention policy has expired independently of runtime topology health.
type WorkspaceRetentionReconciler struct {
	client.Client
	// APIReader bypasses the informer cache for destructive lifecycle decisions.
	APIReader client.Reader
	Runtime   processruntime.Runtime
	// Now allows deterministic deadline and restart tests; defaults to time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaceexecs,verbs=get;list;watch

// Reconcile advances command cleanup or the idle -> suspended -> deleted policy.
// It reads process state independently of runtime dependencies, and persists
// clocks in status so controller restarts do not reset the recovery window.
func (r *WorkspaceRetentionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, returnedErr error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	workspace := new(workspacesv1alpha1.Workspace)
	if err := reader.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	// A crash leaves the fence closed. Re-evaluate policy on restart, and
	// reopen on cancellation/failure. Once DELETE succeeds, Reopen is a no-op.
	defer func() {
		if workspace.Status.ExecutionAdmissionClosed {
			gate := workspaceadmission.Gate{Client: r.Client, Reader: reader}
			returnedErr = errors.Join(returnedErr, gate.Reopen(ctx, workspace))
		}
	}()
	active, hasProcesses, lastCompletion, err := workspaceProcessState(ctx, reader, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if err := r.recordActivity(ctx, workspace, lastCompletion, active, now); err != nil {
		return ctrl.Result{}, err
	}
	if active {
		return r.reconcileExecutions(ctx, workspace)
	}
	if workspace.Spec.IsTemporary() {
		lifecycleResult, lifecycleErr := r.reconcileTemporary(ctx, workspace, hasProcesses, lastCompletion, now)
		executionResult, executionErr := r.reconcileExecutions(ctx, workspace)
		return earlierResult(lifecycleResult, executionResult), errors.Join(lifecycleErr, executionErr)
	}
	if workspace.Spec.DesiredState == workspacesv1alpha1.WorkspaceDesiredStateSuspended {
		lifecycleResult, lifecycleErr := r.reconcileSuspended(ctx, workspace, now)
		executionResult, executionErr := r.reconcileExecutions(ctx, workspace)
		return earlierResult(lifecycleResult, executionResult), errors.Join(lifecycleErr, executionErr)
	}
	if workspace.Spec.IdleTimeout == nil || workspace.Spec.IdleTimeout.Duration <= 0 {
		return r.reconcileExecutions(ctx, workspace)
	}
	deadline := workspace.Status.LastActivityTime.Add(workspace.Spec.IdleTimeout.Duration)
	if remaining := deadline.Sub(now); remaining > 0 {
		executionResult, executionErr := r.reconcileExecutions(ctx, workspace)
		return earlierResult(ctrl.Result{RequeueAfter: remaining}, executionResult), executionErr
	}
	// Updating the version we read prevents a concurrent resume or policy edit
	// from being overwritten. The runtime controller rechecks active executions
	// before stopping compute.
	workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
	if err := r.Update(ctx, workspace); err != nil {
		return ctrl.Result{}, fmt.Errorf("suspend idle Workspace: %w", err)
	}
	logf.FromContext(ctx).Info("Requested idle Workspace suspension", "name", workspace.Name)
	return ctrl.Result{}, nil
}

// recordActivity advances the durable clock, including when execution history
// is later deleted. Attach is deliberately not activity: an active execution
// already protects the Workspace, even when detached or without a terminal.
func (r *WorkspaceRetentionReconciler) recordActivity(ctx context.Context, workspace *workspacesv1alpha1.Workspace, completion *metav1.Time, active bool, now time.Time) error {
	before := workspace.DeepCopy()
	activity := workspace.CreationTimestamp.Time
	if previous := workspace.Status.LastActivityTime; previous != nil && previous.After(activity) {
		activity = previous.Time
	}
	ready := meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
	if ready != nil && ready.Status == metav1.ConditionTrue && ready.LastTransitionTime.After(activity) {
		activity = ready.LastTransitionTime.Time
	}
	if completion != nil && completion.After(activity) {
		activity = completion.Time
	}
	if workspace.Status.SuspendedAt != nil && (workspace.Spec.DesiredState != workspacesv1alpha1.WorkspaceDesiredStateSuspended || active) {
		workspace.Status.SuspendedAt = nil
		activity = now
	}
	if activity.IsZero() {
		activity = now
	}
	stamp := metav1.NewTime(activity)
	if workspace.Status.LastActivityTime.Equal(&stamp) && before.Status.SuspendedAt.Equal(workspace.Status.SuspendedAt) {
		return nil
	}
	workspace.Status.LastActivityTime = &stamp
	return r.Status().Patch(ctx, workspace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *WorkspaceRetentionReconciler) reconcileTemporary(ctx context.Context, workspace *workspacesv1alpha1.Workspace, hasProcesses bool, completion *metav1.Time, now time.Time) (ctrl.Result, error) {
	deadline := workspace.CreationTimestamp.Add(temporaryWorkspaceStartTimeout)
	if hasProcesses {
		// Fail closed until terminal status includes an authoritative completion
		// time; creation/Ready timestamps cannot measure the terminal grace period.
		if completion == nil {
			return ctrl.Result{RequeueAfter: temporaryWorkspaceCleanupDelay}, nil
		}
		deadline = completion.Add(temporaryWorkspaceCleanupDelay)
	}
	if remaining := deadline.Sub(now); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	return ctrl.Result{}, r.deleteExpired(ctx, workspace)
}

func (r *WorkspaceRetentionReconciler) reconcileSuspended(ctx context.Context, workspace *workspacesv1alpha1.Workspace, now time.Time) (ctrl.Result, error) {
	if workspace.Spec.DeleteAfterSuspended == nil || workspace.Spec.DeleteAfterSuspended.Duration <= 0 {
		return ctrl.Result{}, nil
	}
	ready := meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reasonSuspended || ready.ObservedGeneration != workspace.Generation {
		return ctrl.Result{}, nil
	}
	if workspace.Status.SuspendedAt == nil {
		// Legacy Suspended objects have no trustworthy timestamp: Ready=False may
		// have started at Stopping or a failure. Grant the complete recovery window.
		before := workspace.DeepCopy()
		stamp := metav1.NewTime(now)
		workspace.Status.SuspendedAt = &stamp
		if err := r.Status().Patch(ctx, workspace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	deadline := workspace.Status.SuspendedAt.Add(workspace.Spec.DeleteAfterSuspended.Duration)
	if remaining := deadline.Sub(now); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	return ctrl.Result{}, r.deleteExpired(ctx, workspace)
}

func (r *WorkspaceRetentionReconciler) reconcileExecutions(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (ctrl.Result, error) {
	policy := workspace.Spec.ExecutionRetention
	if workspace.Spec.IsTemporary() {
		policy = nil
	} else if policy == nil {
		return ctrl.Result{}, nil
	}
	return (&executionRetentionService{Client: r.Client, APIReader: r.APIReader, Runtime: r.Runtime}).reconcileTarget(ctx, workspace, policy)
}

func earlierResult(a, b ctrl.Result) ctrl.Result {
	result := ctrl.Result{}
	if a.RequeueAfter > 0 && (b.RequeueAfter == 0 || a.RequeueAfter < b.RequeueAfter) {
		result.RequeueAfter = a.RequeueAfter
	} else {
		result.RequeueAfter = b.RequeueAfter
	}
	return result
}

func (r *WorkspaceRetentionReconciler) deleteExpired(ctx context.Context, workspace *workspacesv1alpha1.Workspace) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	gate := workspaceadmission.Gate{Client: r.Client, Reader: reader}
	if err := gate.Close(ctx, workspace); err != nil {
		return err
	}
	// Close before scanning: no execution can now cross controller admission.
	// A process may have appeared while recording activity. Refresh the list at
	// the deletion boundary and fail closed on errors or any nonterminal phase.
	active, _, _, stateErr := workspaceProcessState(ctx, reader, workspace)
	if stateErr != nil {
		return stateErr
	}
	if active {
		return nil
	}
	// Keep the closed gate's version: reopening or a concurrent spec edit
	// invalidates the closed snapshot, and admitted executions block the scan. UID also
	// prevents deleting a replacement object with the same name.
	err := r.Delete(ctx, workspace, client.Preconditions{UID: &workspace.UID, ResourceVersion: &workspace.ResourceVersion})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete expired Workspace: %w", err)
	}
	logf.FromContext(ctx).Info("Requested expired Workspace deletion", "name", workspace.Name)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceRetentionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.Workspace{}).
		Watches(&workspacesv1alpha1.WorkspaceExec{}, handler.EnqueueRequestsFromMapFunc(retentionTargetForProcess(workspacesv1alpha1.WorkspaceExecTargetWorkspace))).
		Owns(&corev1.Pod{}).
		Named("workspaces-workspace-retention").
		Complete(r)
}
