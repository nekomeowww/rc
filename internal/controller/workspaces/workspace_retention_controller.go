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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/rcplatform"
	"github.com/nekomeowww/rc/internal/workspaceadmission"
)

const (
	temporaryWorkspaceCleanupDelay = 5 * time.Minute
	temporaryWorkspaceStartTimeout = 15 * time.Minute
	// workspaceStartTimeout is the least idle time a starting Workspace gets
	// before it is Ready, so a short idleTimeout does not cut a cold start.
	workspaceStartTimeout = 15 * time.Minute
	// runtimeActivityTimeout bounds the activity query at an idle deadline.
	// A runtime that accepts the exec but never answers must not hold the
	// retention worker, which every Workspace's deadlines share.
	runtimeActivityTimeout = 15 * time.Second
)

// WorkspaceRetentionReconciler applies idle suspension and deletes Workspaces
// whose retention policy has expired independently of runtime topology health.
type WorkspaceRetentionReconciler struct {
	client.Client
	// APIReader is required. It bypasses the informer cache for destructive
	// lifecycle decisions.
	APIReader client.Reader
	// History prunes each running supervisor's finished process records.
	History HistoryPruner
	// Activity reads the work a running supervisor recorded, such as tool
	// calls served by a resident agent. It is asked only before an idle
	// suspension, never polled. Nil disables the check.
	Activity ActivityReader
	// Now allows deterministic deadline and restart tests; defaults to time.Now.
	Now func() time.Time

	retention     *historyRetention
	retentionOnce sync.Once
}

// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaceexecs,verbs=get;list;watch;update;patch

// Reconcile advances the idle -> suspended -> deleted policy and history
// retention on the running supervisor.
// It reads process state independently of runtime dependencies, and persists
// clocks in status so controller restarts do not reset the recovery window.
func (r *WorkspaceRetentionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, returnedErr error) {
	workspace := new(workspacesv1alpha1.Workspace)
	if err := r.APIReader.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	// A crash leaves the fence closed. Re-evaluate policy on restart, and
	// reopen on cancellation/failure. Once DELETE succeeds, Reopen is a no-op.
	defer func() {
		if workspace.Status.ExecutionAdmissionClosed {
			gate := workspaceadmission.Gate{Client: r.Client, Reader: r.APIReader}
			returnedErr = errors.Join(returnedErr, gate.Reopen(ctx, workspace))
		}
	}()
	// Planning reads the cache; deleteExpired closes admission and rechecks
	// through the API reader before any destructive request.
	processes, err := listWorkspaceProcesses(ctx, r.Client, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	deadlines, err := r.recordActivity(ctx, workspace, processes, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	var lifecycleResult ctrl.Result
	switch {
	case deadlines.maxRunSuspendAt != nil && !now.Before(*deadlines.maxRunSuspendAt):
		// The maximum run ignores activity: stop running executions so the
		// runtime controller, which waits for them, can stop compute.
		if err := r.stopExecutions(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.requestSuspension(ctx, workspace, workspacesv1alpha1.WorkspaceAutomaticSuspendMaxRunDuration); err != nil {
			return ctrl.Result{}, err
		}
		logf.FromContext(ctx).Info("Requested Workspace suspension at its maximum run duration", "name", workspace.Name)
		return ctrl.Result{}, nil
	case deadlines.idleSuspendAt != nil && !now.Before(*deadlines.idleSuspendAt):
		postponed, err := r.postponeForRuntimeActivity(ctx, workspace, now)
		if err != nil || postponed > 0 {
			return ctrl.Result{RequeueAfter: postponed}, err
		}
		if err := r.requestSuspension(ctx, workspace, workspacesv1alpha1.WorkspaceAutomaticSuspendIdle); err != nil {
			return ctrl.Result{}, err
		}
		logf.FromContext(ctx).Info("Requested idle Workspace suspension", "name", workspace.Name)
		return ctrl.Result{}, nil
	case deadlines.deleteAt != nil && !now.Before(*deadlines.deleteAt):
		deleted, err := r.deleteExpired(ctx, workspace)
		if err != nil || deleted {
			return ctrl.Result{}, err
		}
	default:
		lifecycleResult = ctrl.Result{RequeueAfter: deadlines.requeueAfter(now)}
	}
	r.retentionOnce.Do(func() { r.retention = newHistoryRetention(r.APIReader, r.History, r.Now) })
	historyResult := ctrl.Result{RequeueAfter: r.retention.reconcile(ctx, workspace, workspace.Status.RuntimePodName, workspace.Spec.ExecutionRetention)}
	return earlierResult(lifecycleResult, historyResult), nil
}

// recordActivity advances the durable clocks and publishes status.lifecycle
// in one optimistic patch, written only on change. Activity also advances when
// execution history is later deleted. Attach is deliberately not activity: an
// active execution already protects the Workspace, even when detached.
func (r *WorkspaceRetentionReconciler) recordActivity(ctx context.Context, workspace *workspacesv1alpha1.Workspace, processes workspaceProcesses, now time.Time) (workspaceDeadlines, error) {
	before := workspace.DeepCopy()
	active := processes.active > 0
	activity := workspace.CreationTimestamp.Time
	if previous := workspace.Status.LastActivityTime; previous != nil && previous.After(activity) {
		activity = previous.Time
	}
	ready := meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
	if ready != nil && ready.Status == metav1.ConditionTrue && ready.LastTransitionTime.After(activity) {
		activity = ready.LastTransitionTime.Time
	}
	if completion := processes.lastCompletion; completion != nil && completion.After(activity) {
		activity = completion.Time
	}
	r.recordRun(workspace, ready)
	if workspace.Status.SuspendedAt != nil && (workspace.Spec.DesiredState != workspacesv1alpha1.WorkspaceDesiredStateSuspended || active) {
		workspace.Status.SuspendedAt = nil
		activity = now
	}
	if activity.IsZero() {
		activity = now
	}
	if !workspace.Status.LastActivityTime.Equal(&metav1.Time{Time: activity}) {
		stamp := metav1.NewTime(activity)
		workspace.Status.LastActivityTime = &stamp
	}
	// Legacy Suspended objects have no trustworthy timestamp: Ready=False may
	// have started at Stopping or a failure. Grant the complete recovery window.
	if !active && !workspace.Spec.IsTemporary() && workspace.Spec.DesiredState == workspacesv1alpha1.WorkspaceDesiredStateSuspended &&
		workspace.Status.SuspendedAt == nil && suspendedDeletionEnabled(workspace) {
		stamp := metav1.NewTime(now)
		workspace.Status.SuspendedAt = &stamp
	}
	deadlines := lifecycleDeadlines(workspace, processes)
	workspace.Status.Lifecycle = deadlines.status(processes.active, before.Status.Lifecycle)
	if apiequality.Semantic.DeepEqual(before.Status, workspace.Status) {
		return deadlines, nil
	}
	return deadlines, r.Status().Patch(ctx, workspace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// recordRun starts the run clock at the first Ready of a run and clears it
// once suspension is confirmed. A runtime Pod restart within a run keeps it,
// so restarts cannot extend the maximum run.
func (r *WorkspaceRetentionReconciler) recordRun(workspace *workspacesv1alpha1.Workspace, ready *metav1.Condition) {
	switch {
	case workspace.Spec.DesiredState == workspacesv1alpha1.WorkspaceDesiredStateSuspended:
		if ready != nil && ready.Status == metav1.ConditionFalse && ready.Reason == reasonSuspended {
			workspace.Status.RunningSince = nil
		}
	case workspace.Status.RunningSince == nil && ready != nil && ready.Status == metav1.ConditionTrue:
		started := ready.LastTransitionTime
		workspace.Status.RunningSince = &started
	}
}

// postponeForRuntimeActivity asks the running supervisor for activity it
// recorded, such as tool calls served by a resident agent, before an idle
// suspension. Newer activity advances the idle clock, and the returned wait
// is the new deadline. A query failure keeps the Workspace running and is
// retried; maxRunDuration bounds how long that can last.
func (r *WorkspaceRetentionReconciler) postponeForRuntimeActivity(ctx context.Context, workspace *workspacesv1alpha1.Workspace, now time.Time) (time.Duration, error) {
	if r.Activity == nil || workspace.Status.RuntimePodName == "" || workspace.Spec.IdleTimeout == nil {
		return 0, nil
	}
	pod := new(corev1.Pod)
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: workspace.Namespace, Name: workspace.Status.RuntimePodName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("get runtime Pod for activity: %w", err)
	}
	if pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
		return 0, nil
	}
	platform, err := rcplatform.FromPod(pod)
	if err != nil {
		return 0, fmt.Errorf("resolve runtime platform for activity: %w", err)
	}
	queryCtx, cancel := context.WithTimeout(ctx, runtimeActivityTimeout)
	defer cancel()
	activity, err := r.Activity.LastActivity(queryCtx, platform.ProcessTarget(workspace.Namespace, pod.Name, runtimeContainerName))
	if errors.Is(err, processruntime.ErrActivityUnsupported) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read runtime activity: %w", err)
	}
	if activity.LastActivity == nil || workspace.Status.LastActivityTime == nil || !activity.LastActivity.After(workspace.Status.LastActivityTime.Time) {
		return 0, nil
	}
	deadline := activity.LastActivity.Add(workspace.Spec.IdleTimeout.Duration)
	if !deadline.After(now) {
		return 0, nil
	}
	before := workspace.DeepCopy()
	stamp := metav1.NewTime(*activity.LastActivity)
	workspace.Status.LastActivityTime = &stamp
	if workspace.Status.Lifecycle != nil {
		workspace.Status.Lifecycle.IdleSuspendAt = ceilSecond(&deadline)
	}
	if err := r.Status().Patch(ctx, workspace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return 0, fmt.Errorf("record runtime activity: %w", err)
	}
	logf.FromContext(ctx).V(1).Info("Postponed idle Workspace suspension for runtime activity", "name", workspace.Name, "idleSuspendAt", deadline)
	return deadline.Sub(now), nil
}

// requestSuspension asks the runtime controller to stop compute and records
// why. Updating the version we read prevents a concurrent resume or policy
// edit from being overwritten.
func (r *WorkspaceRetentionReconciler) requestSuspension(ctx context.Context, workspace *workspacesv1alpha1.Workspace, reason string) error {
	workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
	if err := r.Update(ctx, workspace); err != nil {
		return fmt.Errorf("suspend Workspace: %w", err)
	}
	before := workspace.DeepCopy()
	if workspace.Status.Lifecycle == nil {
		workspace.Status.Lifecycle = &workspacesv1alpha1.WorkspaceLifecycleStatus{}
	}
	workspace.Status.Lifecycle.LastAutomaticSuspendReason = reason
	if err := r.Status().Patch(ctx, workspace, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("record automatic suspend reason: %w", err)
	}
	return nil
}

// stopExecutions asks every running execution of the Workspace to stop.
func (r *WorkspaceRetentionReconciler) stopExecutions(ctx context.Context, workspace *workspacesv1alpha1.Workspace) error {
	processes := new(workspacesv1alpha1.WorkspaceExecList)
	if err := r.List(ctx, processes, client.InNamespace(workspace.Namespace), client.MatchingFields{executionTargetIndex: workspace.Name}); err != nil {
		return fmt.Errorf("list Workspace processes: %w", err)
	}
	for index := range processes.Items {
		process := &processes.Items[index]
		if process.Spec.TargetRef.Kind != workspacesv1alpha1.WorkspaceExecTargetWorkspace || process.Spec.TargetRef.Name != workspace.Name ||
			process.Status.Phase.Terminal() || process.Spec.DesiredState == workspacesv1alpha1.WorkspaceExecDesiredStateStopped {
			continue
		}
		before := process.DeepCopy()
		process.Spec.DesiredState = workspacesv1alpha1.WorkspaceExecDesiredStateStopped
		if err := r.Patch(ctx, process, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("stop WorkspaceExec %s: %w", process.Name, err)
		}
		logf.FromContext(ctx).Info("Stopped WorkspaceExec at the Workspace maximum run duration", "name", process.Name, "workspace", workspace.Name)
	}
	return nil
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

// deleteExpired returns true once DELETE was accepted or the Workspace is gone.
func (r *WorkspaceRetentionReconciler) deleteExpired(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (bool, error) {
	gate := workspaceadmission.Gate{Client: r.Client, Reader: r.APIReader}
	if err := gate.Close(ctx, workspace); err != nil {
		return false, err
	}
	// Close before scanning: no execution can now cross controller admission.
	// A process may have appeared while recording activity. Refresh the list at
	// the deletion boundary and fail closed on errors or any nonterminal phase.
	active, _, _, stateErr := workspaceProcessState(ctx, r.APIReader, workspace)
	if stateErr != nil {
		return false, stateErr
	}
	if active {
		return false, nil
	}
	// Keep the closed gate's version: reopening or a concurrent spec edit
	// invalidates the closed snapshot, and admitted executions block the scan. UID also
	// prevents deleting a replacement object with the same name.
	err := r.Delete(ctx, workspace, client.Preconditions{UID: &workspace.UID, ResourceVersion: &workspace.ResourceVersion})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("delete expired Workspace: %w", err)
	}
	logf.FromContext(ctx).Info("Requested expired Workspace deletion", "name", workspace.Name)
	return true, nil
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
