package audit

import (
	"context"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/executionretention"
)

// Retention policy states of an execution target, read from its spec with the
// controller's executionretention.PolicyFor.
const (
	// RetentionConfigured means the controller removes history under spec.executionRetention.
	RetentionConfigured = "Configured"
	// RetentionUnset means spec.executionRetention is omitted; history is kept.
	RetentionUnset = "Unset"
	// RetentionTemporary means history is removed together with a temporary Workspace.
	RetentionTemporary = "Temporary"
	// RetentionTargetDeleting means history is removed together with its target.
	RetentionTargetDeleting = "TargetDeleting"
	// RetentionUnknown means the target kind could not be listed.
	RetentionUnknown = "Unknown"
)

// States of a target's published status.executionHistory.
const (
	// HistoryStatusCurrent means the summary reflects the current target generation.
	HistoryStatusCurrent = "Current"
	// HistoryStatusStale means the summary reflects an older target generation.
	HistoryStatusStale = "Stale"
	// HistoryStatusUnpublished means the controller has not published a summary.
	HistoryStatusUnpublished = "Unpublished"
	// HistoryStatusNotApplicable means the controller publishes no summary for
	// this target, because its history goes with the target.
	HistoryStatusNotApplicable = "NotApplicable"
)

// HistoryReport is the read-only execution history backlog behind rcctl prune.
// Cleanup is performed only by the controller.
type HistoryReport struct {
	Namespace  string          `json:"namespace"`
	ObservedAt metav1.Time     `json:"observedAt"`
	Complete   bool            `json:"complete"`
	Coverage   []Observation   `json:"coverage"`
	Targets    []HistoryTarget `json:"targets"`
}

// HistoryTarget is the controller-published WorkspaceExec history summary of
// one Workspace or WorkspaceEnvironment. Counts are absent unless the
// controller published them; Status tells whether they match the current spec.
type HistoryTarget struct {
	Target              ObjectRef        `json:"target"`
	Retention           string           `json:"retention"`
	Status              string           `json:"status"`
	Reason              string           `json:"reason,omitempty"`
	Message             string           `json:"message,omitempty"`
	Retained            *int32           `json:"retained,omitempty"`
	PendingCleanup      *int32           `json:"pendingCleanup,omitempty"`
	EffectiveTTL        *metav1.Duration `json:"effectiveTTL,omitempty"`
	EffectiveMaxEntries int32            `json:"effectiveMaxEntries,omitempty"`
}

// ExecutionHistory lists Workspaces and WorkspaceEnvironments in namespace (all
// namespaces when empty) and reports each target's published backlog. It never
// writes and never lists WorkspaceExecs. Callers must provide an uncached
// client.Reader.
func ExecutionHistory(ctx context.Context, reader client.Reader, namespace string, now time.Time) (HistoryReport, error) {
	report := HistoryReport{Namespace: namespace, ObservedAt: metav1.NewTime(now.UTC().Truncate(time.Second)), Complete: true, Coverage: []Observation{}, Targets: []HistoryTarget{}}
	var workspaceList workspaces.WorkspaceList
	var environmentList workspaces.WorkspaceEnvironmentList
	for _, item := range []struct {
		list client.ObjectList
		kind string
	}{
		{&workspaceList, workspaceKind},
		{&environmentList, workspaceEnvironmentKind},
	} {
		if err := ctx.Err(); err != nil {
			return HistoryReport{}, err
		}
		observation := Observation{Kind: item.kind, APIVersion: workspaceAPI, Complete: true}
		if err := reader.List(ctx, item.list, client.InNamespace(namespace)); err != nil {
			observation.Complete, observation.Error, report.Complete = false, err.Error(), false
		}
		report.Coverage = append(report.Coverage, observation)
	}
	for i := range workspaceList.Items {
		target := &workspaceList.Items[i]
		report.Targets = append(report.Targets, summarizeTarget(target, target.Status.ExecutionHistory, target.Status.Conditions))
	}
	for i := range environmentList.Items {
		target := &environmentList.Items[i]
		report.Targets = append(report.Targets, summarizeTarget(target, target.Status.ExecutionHistory, target.Status.Conditions))
	}
	slices.SortFunc(report.Targets, func(a, b HistoryTarget) int { return strings.Compare(key(a.Target), key(b.Target)) })
	return report, nil
}

// summarizeTarget reads the target's status.executionHistory and
// ExecutionHistoryCompliant condition. It never recomputes the backlog: an
// absent or older summary is reported as Unpublished or Stale.
func summarizeTarget(target client.Object, history *workspaces.ExecutionHistoryStatus, conditions []metav1.Condition) HistoryTarget {
	kind := workspaceKind
	if _, ok := target.(*workspaces.WorkspaceEnvironment); ok {
		kind = workspaceEnvironmentKind
	}
	summary := HistoryTarget{Target: ObjectRef{APIVersion: workspaceAPI, Kind: kind, Namespace: target.GetNamespace(), Name: target.GetName(), UID: target.GetUID()}}
	policy, applies := executionretention.PolicyFor(target)
	switch {
	case !target.GetDeletionTimestamp().IsZero():
		summary.Retention = RetentionTargetDeleting
	case !applies:
		summary.Retention = RetentionTemporary
	case policy == nil:
		summary.Retention = RetentionUnset
	default:
		summary.Retention = RetentionConfigured
	}
	switch {
	case !applies:
		summary.Status = HistoryStatusNotApplicable
		return summary
	case history == nil:
		summary.Status = HistoryStatusUnpublished
		summary.Message = "The controller has not published status.executionHistory; the backlog is unknown"
		return summary
	case history.ObservedGeneration < target.GetGeneration():
		summary.Status = HistoryStatusStale
		summary.Message = "status.executionHistory reflects an older spec generation"
	default:
		summary.Status = HistoryStatusCurrent
	}
	summary.Retained, summary.PendingCleanup = &history.Retained, &history.PendingCleanup
	summary.EffectiveTTL, summary.EffectiveMaxEntries = history.EffectiveTTL, history.EffectiveMaxEntries
	if condition := meta.FindStatusCondition(conditions, workspaces.ConditionExecutionHistoryCompliant); condition != nil {
		summary.Reason = condition.Reason
		summary.Message = strings.TrimPrefix(summary.Message+"; "+condition.Message, "; ")
	}
	return summary
}
