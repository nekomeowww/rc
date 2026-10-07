// Package executionretention defines the canonical age and count policy for
// terminal WorkspaceExec history. Callers remain responsible for storage
// cleanup and mutation fencing.
package executionretention

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultTTL applies when TTLAfterFinished is unset or not positive.
	DefaultTTL = 90 * 24 * time.Hour
	// DefaultMaxEntries applies when MaxEntries is not positive.
	DefaultMaxEntries = 15000
	// DefaultTranscriptTTL applies when TranscriptTTL is unset. An explicit
	// zero TranscriptTTL expires transcripts immediately.
	DefaultTranscriptTTL = 14 * 24 * time.Hour
)

// Plan identifies retained and removable entries by index. The input order is
// never changed.
type Plan struct {
	Keep   []int
	Remove []int
}

// PolicyFor resolves a target's history-pruning policy. ok is false for absent,
// deleting, temporary, and unsupported targets: they have no history-pruning
// policy and retain their whole-target deletion lifecycle. A nil policy with ok
// true means the target keeps all history.
func PolicyFor(target client.Object) (policy *workspacesv1alpha1.ExecutionRetentionPolicy, ok bool) {
	if target == nil || !target.GetDeletionTimestamp().IsZero() {
		return nil, false
	}
	switch target := target.(type) {
	case *workspacesv1alpha1.Workspace:
		if target.Spec.IsTemporary() {
			return nil, false
		}
		return target.Spec.ExecutionRetention, true
	case *workspacesv1alpha1.WorkspaceEnvironment:
		return target.Spec.ExecutionRetention, true
	default:
		return nil, false
	}
}

// BuildForTarget resolves a target's effective policy and applies it to a
// complete target-scoped execution snapshot. Unsupported, deleting, and
// temporary targets fail closed because they have no history-pruning policy.
func BuildForTarget(executions []workspacesv1alpha1.WorkspaceExec, target client.Object, now time.Time) (Plan, error) {
	policy, ok := PolicyFor(target)
	if !ok {
		return Plan{}, fmt.Errorf("execution target is absent, deleting, temporary, or unsupported (%T); it has no history-pruning policy", target)
	}
	return Build(executions, policy, now), nil
}

// Build applies the canonical retention defaults, pins, terminal-state checks,
// TTL, and per-target count limit to one target-scoped snapshot.
func Build(executions []workspacesv1alpha1.WorkspaceExec, policy *workspacesv1alpha1.ExecutionRetentionPolicy, now time.Time) Plan {
	plan := Plan{}
	eligible := make([]int, 0, len(executions))
	for i := range executions {
		process := &executions[i]
		if policy == nil || process.Spec.Retain || !process.Status.Phase.Terminal() || process.Status.CompletedAt == nil || !process.DeletionTimestamp.IsZero() {
			plan.Keep = append(plan.Keep, i)
			continue
		}
		eligible = append(eligible, i)
	}
	if policy == nil {
		return plan
	}
	ttl, maximum := DefaultTTL, policy.MaxEntries
	if policy.TTLAfterFinished != nil && policy.TTLAfterFinished.Duration > 0 {
		ttl = policy.TTLAfterFinished.Duration
	}
	if maximum <= 0 {
		maximum = DefaultMaxEntries
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
