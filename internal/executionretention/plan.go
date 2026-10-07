// Package executionretention defines the canonical age and count policy for
// terminal WorkspaceExec history. Callers remain responsible for storage
// cleanup and mutation fencing.
package executionretention

import (
	"cmp"
	"slices"
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// Plan identifies retained and removable entries by index. The input order is
// never changed.
type Plan struct {
	Keep   []int
	Remove []int
}

// Build applies the canonical retention defaults, pins, terminal-state checks,
// TTL, and per-target count limit to one target-scoped snapshot.
func Build(executions []workspacesv1alpha1.WorkspaceExec, policy *workspacesv1alpha1.ExecutionRetentionPolicy, now time.Time) Plan {
	plan := Plan{}
	eligible := make([]int, 0, len(executions))
	for i := range executions {
		process := &executions[i]
		if policy == nil || process.Spec.Retain || !Terminal(process.Status.Phase) || process.Status.CompletedAt == nil || !process.DeletionTimestamp.IsZero() {
			plan.Keep = append(plan.Keep, i)
			continue
		}
		eligible = append(eligible, i)
	}
	if policy == nil {
		return plan
	}
	ttl, maximum := 7*24*time.Hour, policy.MaxEntries
	if policy.TTLAfterFinished != nil {
		ttl = policy.TTLAfterFinished.Duration
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	if maximum <= 0 {
		maximum = 500
	}
	slices.SortFunc(eligible, func(a, b int) int {
		if order := executions[b].Status.CompletedAt.Compare(executions[a].Status.CompletedAt.Time); order != 0 {
			return order
		}
		return cmp.Compare(executions[a].Name, executions[b].Name)
	})
	for rank, i := range eligible {
		process := &executions[i]
		if rank >= int(maximum) || !now.Before(process.Status.CompletedAt.Add(ttl)) {
			plan.Remove = append(plan.Remove, i)
			continue
		}
		plan.Keep = append(plan.Keep, i)
	}
	// Delete oldest first, including when reducing an existing target's limit.
	slices.Reverse(plan.Remove)
	return plan
}

// Terminal reports whether an execution phase can no longer make progress.
func Terminal(phase workspacesv1alpha1.WorkspaceExecPhase) bool {
	switch phase {
	case workspacesv1alpha1.WorkspaceExecPhaseSucceeded, workspacesv1alpha1.WorkspaceExecPhaseFailed, workspacesv1alpha1.WorkspaceExecPhaseStopped, workspacesv1alpha1.WorkspaceExecPhaseLost:
		return true
	default:
		return false
	}
}
