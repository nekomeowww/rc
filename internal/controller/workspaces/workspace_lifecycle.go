package workspaces

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// workspaceDeadlines are the retention controller's next automatic actions.
// The same values drive RequeueAfter and status.lifecycle.
type workspaceDeadlines struct {
	idleSuspendAt *time.Time
	deleteAt      *time.Time
	// maxRunSuspendAt is not blocked by active executions: it bounds work
	// that never goes idle.
	maxRunSuspendAt *time.Time
	// recheck bounds the wait when a deadline is not known yet.
	recheck time.Duration
}

// lifecycleDeadlines is the single place that derives the automatic deadlines
// from persisted clocks. Active executions block every stage except the
// maximum run. Callers persist lastActivityTime, runningSince, and a legacy
// suspendedAt stamp first.
func lifecycleDeadlines(workspace *workspacesv1alpha1.Workspace, processes workspaceProcesses) workspaceDeadlines {
	deadlines := idleAndDeleteDeadlines(workspace, processes)
	deadlines.maxRunSuspendAt = maxRunDeadline(workspace)
	return deadlines
}

// maxRunDeadline is when the current run reaches maxRunDuration.
func maxRunDeadline(workspace *workspacesv1alpha1.Workspace) *time.Time {
	if workspace.Spec.DesiredState == workspacesv1alpha1.WorkspaceDesiredStateSuspended ||
		workspace.Spec.MaxRunDuration == nil || workspace.Spec.MaxRunDuration.Duration <= 0 || workspace.Status.RunningSince == nil {
		return nil
	}
	deadline := workspace.Status.RunningSince.Add(workspace.Spec.MaxRunDuration.Duration)
	return &deadline
}

func idleAndDeleteDeadlines(workspace *workspacesv1alpha1.Workspace, processes workspaceProcesses) workspaceDeadlines {
	if processes.active > 0 {
		return workspaceDeadlines{}
	}
	switch {
	case workspace.Spec.IsTemporary():
		deadline := workspace.CreationTimestamp.Add(temporaryWorkspaceStartTimeout)
		if processes.any {
			// Fail closed until terminal status includes an authoritative completion
			// time; creation/Ready timestamps cannot measure the terminal grace period.
			if processes.lastCompletion == nil {
				return workspaceDeadlines{recheck: temporaryWorkspaceCleanupDelay}
			}
			deadline = processes.lastCompletion.Add(temporaryWorkspaceCleanupDelay)
		}
		return workspaceDeadlines{deleteAt: &deadline}
	case workspace.Spec.DesiredState == workspacesv1alpha1.WorkspaceDesiredStateSuspended:
		if !suspendedDeletionEnabled(workspace) || workspace.Status.SuspendedAt == nil {
			return workspaceDeadlines{}
		}
		deadline := workspace.Status.SuspendedAt.Add(workspace.Spec.DeleteAfterSuspended.Duration)
		return workspaceDeadlines{deleteAt: &deadline}
	case workspace.Spec.IdleTimeout == nil || workspace.Spec.IdleTimeout.Duration <= 0 || workspace.Status.LastActivityTime == nil:
		return workspaceDeadlines{}
	default:
		deadline := workspace.Status.LastActivityTime.Add(workspace.Spec.IdleTimeout.Duration)
		return workspaceDeadlines{idleSuspendAt: &deadline}
	}
}

// suspendedDeletionEnabled requires the opt-in and a confirmed suspension at
// the current generation. Stopping and failures share Ready=False.
func suspendedDeletionEnabled(workspace *workspacesv1alpha1.Workspace) bool {
	if workspace.Spec.DeleteAfterSuspended == nil || workspace.Spec.DeleteAfterSuspended.Duration <= 0 {
		return false
	}
	ready := meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
	return ready != nil && ready.Status == metav1.ConditionFalse && ready.Reason == reasonSuspended && ready.ObservedGeneration == workspace.Generation
}

// requeueAfter returns the wait until the earliest pending deadline, or zero
// when no automatic action is scheduled. Due deadlines are acted on by the
// caller and are never requeued here.
func (d workspaceDeadlines) requeueAfter(now time.Time) time.Duration {
	wait := d.recheck
	for _, deadline := range []*time.Time{
		d.idleSuspendAt,
		d.deleteAt,
		d.maxRunSuspendAt,
	} {
		if deadline == nil {
			continue
		}
		if remaining := deadline.Sub(now); remaining > 0 && (wait == 0 || remaining < wait) {
			wait = remaining
		}
	}
	return wait
}

// status publishes the deadlines. API times have second precision, so round up:
// a published deadline never precedes the action, and an unchanged deadline
// compares equal after a round trip. The last automatic suspend reason is
// history, not a deadline, so it is carried over from previous.
func (d workspaceDeadlines) status(active int32, previous *workspacesv1alpha1.WorkspaceLifecycleStatus) *workspacesv1alpha1.WorkspaceLifecycleStatus {
	status := &workspacesv1alpha1.WorkspaceLifecycleStatus{
		IdleSuspendAt:    ceilSecond(d.idleSuspendAt),
		DeleteAt:         ceilSecond(d.deleteAt),
		MaxRunSuspendAt:  ceilSecond(d.maxRunSuspendAt),
		ActiveExecutions: active,
	}
	if previous != nil {
		status.LastAutomaticSuspendReason = previous.LastAutomaticSuspendReason
	}
	return status
}

func ceilSecond(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	rounded := t.Truncate(time.Second)
	if rounded.Before(*t) {
		rounded = rounded.Add(time.Second)
	}
	stamp := metav1.NewTime(rounded.UTC())
	return &stamp
}
