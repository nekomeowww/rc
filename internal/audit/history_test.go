package audit

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

type requestCounts struct{ lists, gets int }

// readOnlyClient fails the test on any write. ExecutionHistory must never
// delete history: cleanup belongs to the controller.
func readOnlyClient(t *testing.T, counts *requestCounts, objects ...client.Object) client.Client {
	t.Helper()
	write := func(verb string) error {
		t.Errorf("read-only history report issued %s", verb)
		return assert.AnError
	}
	return interceptor.NewClient(fixtureClient(t, objects...), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			counts.lists++
			return c.List(ctx, list, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			counts.gets++
			return c.Get(ctx, k, obj, opts...)
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return write("CREATE")
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return write("UPDATE")
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return write("PATCH")
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return write("DELETE")
		},
		DeleteAllOf: func(context.Context, client.WithWatch, client.Object, ...client.DeleteAllOfOption) error {
			return write("DELETECOLLECTION")
		},
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return write("UPDATE status")
		},
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return write("PATCH status")
		},
	})
}

func historyFor(t *testing.T, report HistoryReport, kind, name string) HistoryTarget {
	t.Helper()
	for _, target := range report.Targets {
		if target.Target.Kind == kind && target.Target.Name == name {
			return target
		}
	}
	require.FailNow(t, "target not reported", "%s/%s", kind, name)
	return HistoryTarget{}
}

func publishedHistory(generation int64, retained, pending int32, ttl time.Duration) *workspaces.ExecutionHistoryStatus {
	history := &workspaces.ExecutionHistoryStatus{Retained: retained, PendingCleanup: pending, ObservedGeneration: generation}
	if ttl > 0 {
		history.EffectiveTTL, history.EffectiveMaxEntries = &metav1.Duration{Duration: ttl}, 100
	}
	return history
}

func compliant(status metav1.ConditionStatus, reason, message string) []metav1.Condition {
	return []metav1.Condition{{Type: workspaces.ConditionExecutionHistoryCompliant, Status: status, Reason: reason, Message: message, LastTransitionTime: metav1.NewTime(auditNow)}}
}

func TestExecutionHistoryReadsPublishedStatusWithoutWrites(t *testing.T) {
	day := 24 * time.Hour
	policy := &workspaces.ExecutionRetentionPolicy{TTLAfterFinished: &metav1.Duration{Duration: day}}
	backlog := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName), Spec: workspaces.WorkspaceSpec{ExecutionRetention: policy}}
	backlog.Generation = 3
	backlog.Status.ExecutionHistory = publishedHistory(3, 4, 2, day)
	backlog.Status.Conditions = compliant(metav1.ConditionFalse, workspaces.ReasonCleanupBacklog, "2 WorkspaceExec records wait for cleanup")
	stale := &workspaces.Workspace{ObjectMeta: fixtureMeta("stale"), Spec: workspaces.WorkspaceSpec{ExecutionRetention: policy}}
	stale.Generation = 2
	stale.Status.ExecutionHistory = publishedHistory(1, 7, 0, 0)
	stale.Status.Conditions = compliant(metav1.ConditionTrue, workspaces.ReasonPolicyUnset, "No executionRetention policy")
	unpublished := &workspaces.Workspace{ObjectMeta: fixtureMeta("unpublished")}
	temporary := &workspaces.Workspace{ObjectMeta: fixtureMeta("temporary"), Spec: workspaces.WorkspaceSpec{RetentionPolicy: workspaces.WorkspaceRetentionPolicyDeleteAfterProcessesExit}}
	deleting := &workspaces.Workspace{ObjectMeta: fixtureMeta("deleting")}
	deleting.Finalizers, deleting.DeletionTimestamp = []string{"fixture"}, new(metav1.NewTime(auditNow))
	deleting.Status.ExecutionHistory = publishedHistory(0, 9, 9, 0)
	environment := &workspaces.WorkspaceEnvironment{ObjectMeta: fixtureMeta("env")}
	environment.Status.ExecutionHistory = publishedHistory(0, 5, 0, 0)
	environment.Status.Conditions = compliant(metav1.ConditionTrue, workspaces.ReasonPolicyUnset, "history is kept")
	// A WorkspaceExec is visible but never read: the report trusts the target summary.
	objects := []client.Object{backlog, stale, unpublished, temporary, deleting, environment, terminalRecord("expired")}

	counts := &requestCounts{}
	report, err := ExecutionHistory(t.Context(), readOnlyClient(t, counts, objects...), testNamespace, auditNow)
	require.NoError(t, err)
	assert.True(t, report.Complete)
	assert.Equal(t, 2, counts.lists, "one LIST per target kind; WorkspaceExecs are not listed")
	assert.Zero(t, counts.gets)
	assert.Len(t, report.Targets, 6)

	count := func(n int32) *int32 { return &n }
	for _, tc := range []struct {
		kind, name, retention, status, reason string
		retained, pending                     *int32
	}{
		{workspaceKind, testWorkspaceName, RetentionConfigured, HistoryStatusCurrent, workspaces.ReasonCleanupBacklog, count(4), count(2)},
		{workspaceKind, stale.Name, RetentionConfigured, HistoryStatusStale, workspaces.ReasonPolicyUnset, count(7), count(0)},
		{workspaceKind, unpublished.Name, RetentionUnset, HistoryStatusUnpublished, "", nil, nil},
		{workspaceKind, temporary.Name, RetentionTemporary, HistoryStatusNotApplicable, "", nil, nil},
		{workspaceKind, deleting.Name, RetentionTargetDeleting, HistoryStatusNotApplicable, "", nil, nil},
		{workspaceEnvironmentKind, environment.Name, RetentionUnset, HistoryStatusCurrent, workspaces.ReasonPolicyUnset, count(5), count(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := historyFor(t, report, tc.kind, tc.name)
			assert.Equal(t, tc.retention, got.Retention)
			assert.Equal(t, tc.status, got.Status)
			assert.Equal(t, tc.reason, got.Reason)
			assert.Equal(t, tc.retained, got.Retained)
			assert.Equal(t, tc.pending, got.PendingCleanup)
		})
	}
	got := historyFor(t, report, workspaceKind, testWorkspaceName)
	assert.Equal(t, &metav1.Duration{Duration: day}, got.EffectiveTTL)
	assert.EqualValues(t, 100, got.EffectiveMaxEntries)
	assert.Contains(t, historyFor(t, report, workspaceKind, stale.Name).Message, "older spec generation")
	assert.Contains(t, historyFor(t, report, workspaceKind, unpublished.Name).Message, "not published")
}

func TestExecutionHistoryKeepsVisibleTargetsWhenOneKindIsHidden(t *testing.T) {
	counts := &requestCounts{}
	environment := &workspaces.WorkspaceEnvironment{ObjectMeta: fixtureMeta("env")}
	base := readOnlyClient(t, counts, &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName)}, environment)
	kube := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*workspaces.WorkspaceList); ok {
			return assert.AnError
		}
		return c.List(ctx, list, opts...)
	}})
	report, err := ExecutionHistory(t.Context(), kube, testNamespace, auditNow)
	require.NoError(t, err)
	assert.False(t, report.Complete)
	require.Len(t, report.Targets, 1)
	assert.Equal(t, workspaceEnvironmentKind, report.Targets[0].Target.Kind)
	assert.Equal(t, HistoryStatusUnpublished, report.Targets[0].Status)
}
