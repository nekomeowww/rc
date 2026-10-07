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

func completedAgo(record *workspaces.WorkspaceExec, age time.Duration) *workspaces.WorkspaceExec {
	record.Status.CompletedAt = new(metav1.NewTime(auditNow.Add(-age)))
	return record
}

func TestExecutionHistoryReportsBacklogWithoutWrites(t *testing.T) {
	day := 24 * time.Hour
	configured := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName), Spec: workspaces.WorkspaceSpec{ExecutionRetention: &workspaces.ExecutionRetentionPolicy{TTLAfterFinished: &metav1.Duration{Duration: 30 * day}, MaxEntries: 2}}}
	unset := &workspaces.Workspace{ObjectMeta: fixtureMeta("unset")}
	temporary := &workspaces.Workspace{ObjectMeta: fixtureMeta("temporary"), Spec: workspaces.WorkspaceSpec{RetentionPolicy: workspaces.WorkspaceRetentionPolicyDeleteAfterProcessesExit}}
	environment := &workspaces.WorkspaceEnvironment{ObjectMeta: fixtureMeta("env"), Spec: workspaces.WorkspaceEnvironmentSpec{ExecutionRetention: &workspaces.ExecutionRetentionPolicy{TTLAfterFinished: &metav1.Duration{Duration: day}}}}
	retarget := func(record *workspaces.WorkspaceExec, kind workspaces.WorkspaceExecTargetKind, name string) *workspaces.WorkspaceExec {
		record.Spec.TargetRef = workspaces.WorkspaceExecTargetReference{Kind: kind, Name: name}
		return record
	}
	pinned := completedAgo(terminalRecord("pinned"), 60*day)
	pinned.Spec.Retain = true
	running := terminalRecord("running")
	running.Status.Phase, running.Status.CompletedAt = workspaces.WorkspaceExecPhaseRunning, nil
	deleting := completedAgo(terminalRecord("deleting"), 60*day)
	deleting.DeletionTimestamp = new(metav1.NewTime(auditNow))
	objects := []client.Object{
		configured, unset, temporary, environment,
		// configured: one expired by TTL, one evicted by maxEntries=2, two kept.
		completedAgo(terminalRecord("expired"), 40*day),
		completedAgo(terminalRecord("newest"), day),
		completedAgo(terminalRecord("newer"), 2*day),
		completedAgo(terminalRecord("over-limit"), 3*day),
		pinned, running, deleting,
		retarget(completedAgo(terminalRecord("unset-old"), 365*day), workspaces.WorkspaceExecTargetWorkspace, unset.Name),
		retarget(completedAgo(terminalRecord("temporary-old"), 365*day), workspaces.WorkspaceExecTargetWorkspace, temporary.Name),
		retarget(completedAgo(terminalRecord("env-old"), 2*day), workspaces.WorkspaceExecTargetWorkspaceEnvironment, environment.Name),
		retarget(completedAgo(terminalRecord("orphan-old"), 365*day), workspaces.WorkspaceExecTargetWorkspace, "gone"),
	}
	counts := &requestCounts{}
	report, err := ExecutionHistory(t.Context(), readOnlyClient(t, counts, objects...), testNamespace, auditNow)
	require.NoError(t, err)
	assert.True(t, report.Complete)
	assert.Equal(t, 3, counts.lists, "one LIST per kind")
	assert.Zero(t, counts.gets)

	for _, tc := range []struct {
		kind, name, retention       string
		retained, pending, deleting int
	}{
		{workspaceKind, testWorkspaceName, RetentionConfigured, 5, 2, 1},
		{workspaceKind, unset.Name, RetentionUnset, 1, 0, 0},
		{workspaceKind, temporary.Name, RetentionTemporary, 1, 0, 0},
		{workspaceEnvironmentKind, environment.Name, RetentionConfigured, 0, 1, 0},
		{workspaceKind, "gone", RetentionTargetMissing, 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := historyFor(t, report, tc.kind, tc.name)
			assert.Equal(t, tc.retention, got.Retention)
			assert.Equal(t, tc.retained, got.Retained)
			assert.Equal(t, tc.pending, got.PendingCleanup)
			assert.Equal(t, tc.deleting, got.Deleting)
		})
	}
}

func TestExecutionHistoryWithoutExecutionVisibilityReportsNoTargets(t *testing.T) {
	counts := &requestCounts{}
	base := readOnlyClient(t, counts, terminalRecord("done"))
	kube := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*workspaces.WorkspaceExecList); ok {
			return assert.AnError
		}
		return c.List(ctx, list, opts...)
	}})
	report, err := ExecutionHistory(t.Context(), kube, testNamespace, auditNow)
	require.NoError(t, err)
	assert.False(t, report.Complete)
	assert.Empty(t, report.Targets)
}
