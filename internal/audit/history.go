package audit

import (
	"context"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/executionretention"
)

// Retention policy states of an execution target, as the controller sees them.
const (
	// RetentionConfigured means the controller removes history under spec.executionRetention.
	RetentionConfigured = "Configured"
	// RetentionUnset means spec.executionRetention is omitted; history is kept.
	RetentionUnset = "Unset"
	// RetentionTemporary means history is removed together with a temporary Workspace.
	RetentionTemporary = "Temporary"
	// RetentionTargetDeleting means history is removed together with its target.
	RetentionTargetDeleting = "TargetDeleting"
	// RetentionTargetMissing means no visible target owns the history.
	RetentionTargetMissing = "TargetMissing"
	// RetentionUnknown means the target kind could not be listed.
	RetentionUnknown = "Unknown"
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

// HistoryTarget summarizes the WorkspaceExec history of one Workspace or
// WorkspaceEnvironment. PendingCleanup counts terminal records that are past the
// target's policy and wait for the controller to remove them.
type HistoryTarget struct {
	Target         ObjectRef                            `json:"target"`
	Retention      string                               `json:"retention"`
	Policy         *workspaces.ExecutionRetentionPolicy `json:"policy,omitempty"`
	Retained       int                                  `json:"retained"`
	PendingCleanup int                                  `json:"pendingCleanup"`
	Deleting       int                                  `json:"deleting"`
}

// ExecutionHistory lists Workspaces, WorkspaceEnvironments and WorkspaceExecs in
// namespace (all namespaces when empty) and reports each target's backlog. It
// never writes. Callers must provide an uncached client.Reader.
func ExecutionHistory(ctx context.Context, reader client.Reader, namespace string, now time.Time) (HistoryReport, error) {
	report := HistoryReport{Namespace: namespace, ObservedAt: metav1.NewTime(now.UTC().Truncate(time.Second)), Complete: true, Coverage: []Observation{}, Targets: []HistoryTarget{}}
	var workspaceList workspaces.WorkspaceList
	var environmentList workspaces.WorkspaceEnvironmentList
	var executionList workspaces.WorkspaceExecList
	visible := map[string]bool{}
	for _, item := range []struct {
		list client.ObjectList
		kind string
	}{
		{&workspaceList, workspaceKind},
		{&environmentList, workspaceEnvironmentKind},
		{&executionList, workspaceExecKind},
	} {
		if err := ctx.Err(); err != nil {
			return HistoryReport{}, err
		}
		observation := Observation{Kind: item.kind, APIVersion: workspaceAPI, Complete: true}
		if err := reader.List(ctx, item.list, client.InNamespace(namespace)); err != nil {
			observation.Complete, observation.Error, report.Complete = false, err.Error(), false
		}
		visible[item.kind] = observation.Complete
		report.Coverage = append(report.Coverage, observation)
	}
	if !visible[workspaceExecKind] {
		return report, nil
	}
	targets := map[ObjectRef]client.Object{}
	for i := range workspaceList.Items {
		targets[targetRef(workspaceKind, workspaceList.Items[i].Namespace, workspaceList.Items[i].Name)] = &workspaceList.Items[i]
	}
	for i := range environmentList.Items {
		targets[targetRef(workspaceEnvironmentKind, environmentList.Items[i].Namespace, environmentList.Items[i].Name)] = &environmentList.Items[i]
	}
	groups := map[ObjectRef][]workspaces.WorkspaceExec{}
	for _, execution := range executionList.Items {
		ref := targetRef(string(execution.Spec.TargetRef.Kind), execution.Namespace, execution.Spec.TargetRef.Name)
		groups[ref] = append(groups[ref], execution)
	}
	for ref := range targets {
		if _, ok := groups[ref]; !ok {
			groups[ref] = nil
		}
	}
	for ref, executions := range groups {
		summary := summarizeTarget(targets[ref], visible[ref.Kind], executions, now)
		summary.Target = ref
		if target := targets[ref]; target != nil {
			summary.Target.UID = target.GetUID()
		}
		report.Targets = append(report.Targets, summary)
	}
	slices.SortFunc(report.Targets, func(a, b HistoryTarget) int { return strings.Compare(key(a.Target), key(b.Target)) })
	return report, nil
}

func targetRef(kind, namespace, name string) ObjectRef {
	return ObjectRef{APIVersion: workspaceAPI, Kind: kind, Namespace: namespace, Name: name}
}

// summarizeTarget is the single place that derives a target's backlog. It
// applies executionretention.Build, the controller's eligibility rule, to the
// listed snapshot. Once targets publish status.executionHistory, this becomes a
// read of that status.
func summarizeTarget(target client.Object, visible bool, executions []workspaces.WorkspaceExec, now time.Time) HistoryTarget {
	summary := HistoryTarget{}
	summary.Retention, summary.Policy = retentionPolicy(target, visible)
	for i := range executions {
		if !executions[i].DeletionTimestamp.IsZero() {
			summary.Deleting++
		}
	}
	plan := executionretention.Build(executions, summary.Policy, now)
	summary.Retained, summary.PendingCleanup = len(plan.Keep), len(plan.Remove)
	return summary
}

// retentionPolicy mirrors how the controller resolves a target's policy: deleting
// and temporary targets keep their whole-target lifecycle.
func retentionPolicy(target client.Object, visible bool) (string, *workspaces.ExecutionRetentionPolicy) {
	if target == nil {
		if !visible {
			return RetentionUnknown, nil
		}
		return RetentionTargetMissing, nil
	}
	if !target.GetDeletionTimestamp().IsZero() {
		return RetentionTargetDeleting, nil
	}
	var policy *workspaces.ExecutionRetentionPolicy
	switch target := target.(type) {
	case *workspaces.Workspace:
		if target.Spec.IsTemporary() {
			return RetentionTemporary, nil
		}
		policy = target.Spec.ExecutionRetention
	case *workspaces.WorkspaceEnvironment:
		policy = target.Spec.ExecutionRetention
	}
	if policy == nil {
		return RetentionUnset, nil
	}
	return RetentionConfigured, policy
}
