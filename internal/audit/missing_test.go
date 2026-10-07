package audit

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func readyWorkspaceWithMissingDependencies() *workspaces.Workspace {
	return &workspaces.Workspace{
		ObjectMeta: fixtureMeta("stale-ready"),
		Spec:       workspaces.WorkspaceSpec{DesiredState: workspaces.WorkspaceDesiredStateRunning},
		Status: workspaces.WorkspaceStatus{
			RuntimePodName: "lost-runtime", HomeVolumeClaimName: "lost-home",
			Conditions: []metav1.Condition{{Type: workspaces.WorkspaceConditionReady, Status: metav1.ConditionTrue}},
		},
	}
}

func TestMissingRuntimeAndStorageOverrideStaleReady(t *testing.T) {
	// ROOT CAUSE: only existing targets entered the graph. A status reference
	// resolving to nothing was silently skipped, so stale Ready yielded no finding.
	// Missing is justified only by a successful list of the target kind and scope.
	workspace := readyWorkspaceWithMissingDependencies()
	report, err := Scan(t.Context(), fixtureClient(t, workspace), testNamespace, DefaultPolicy(), auditNow)
	require.NoError(t, err)
	require.True(t, report.Inventory.Complete)
	for _, expected := range []struct{ code, kind, name string }{
		{missingRuntimeCode, podKind, workspace.Status.RuntimePodName},
		{missingStorageCode, pvcKind, workspace.Status.HomeVolumeClaimName},
	} {
		t.Run(expected.code, func(t *testing.T) {
			finding := requireFinding(t, report, expected.code)
			assert.Equal(t, "warning", finding.Severity)
			assert.Equal(t, workspace.Name, finding.Resource.Name)
			require.Len(t, finding.Related, 1)
			assert.Equal(t, expected.kind, finding.Related[0].Kind)
			assert.Equal(t, expected.name, finding.Related[0].Name)
			assert.Equal(t, testNamespace, finding.Related[0].Namespace)
		})
	}
}

func TestMissingDependenciesRemainUnknownWhenRBACHidesTargetKind(t *testing.T) {
	base := fixtureClient(t, readyWorkspaceWithMissingDependencies())
	kube := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		switch list.(type) {
		case *corev1.PodList, *corev1.PersistentVolumeClaimList:
			return apierrors.NewForbidden(schema.GroupResource{Resource: "fixture"}, "", assert.AnError)
		default:
			return c.List(ctx, list, opts...)
		}
	}})
	report, err := Scan(t.Context(), kube, testNamespace, DefaultPolicy(), auditNow)
	require.NoError(t, err)
	assert.False(t, report.Inventory.Complete)
	assert.Equal(t, severityUnknown, requireFinding(t, report, "RuntimeUnknown").Severity)
	assert.Equal(t, severityUnknown, requireFinding(t, report, "StorageUnknown").Severity)
	for _, finding := range report.Findings {
		assert.NotEqual(t, missingRuntimeCode, finding.Code)
		assert.NotEqual(t, missingStorageCode, finding.Code)
	}
}

func TestUnrelatedRBACFailureDoesNotHideConfirmedMissingDependencies(t *testing.T) {
	base := fixtureClient(t, readyWorkspaceWithMissingDependencies())
	kube := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.EventList); ok {
			return assert.AnError
		}
		return c.List(ctx, list, opts...)
	}})
	report, err := Scan(t.Context(), kube, testNamespace, DefaultPolicy(), auditNow)
	require.NoError(t, err)
	assert.False(t, report.Inventory.Complete)
	requireFinding(t, report, missingRuntimeCode)
	requireFinding(t, report, missingStorageCode)
}

func TestMissingDependencyCannotBeSatisfiedByAnotherNamespace(t *testing.T) {
	workspace := readyWorkspaceWithMissingDependencies()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: workspace.Status.RuntimePodName, Namespace: "other"}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: workspace.Status.HomeVolumeClaimName, Namespace: "other"}}
	report, err := Scan(t.Context(), fixtureClient(t, workspace, pod, pvc), "", DefaultPolicy(), auditNow)
	require.NoError(t, err)
	requireFinding(t, report, missingRuntimeCode)
	requireFinding(t, report, missingStorageCode)
}

func requireFinding(t *testing.T, report Report, code string) Finding {
	t.Helper()
	for _, finding := range report.Findings {
		if finding.Code == code {
			return finding
		}
	}
	t.Fatalf("expected %s, findings: %+v", code, report.Findings)
	return Finding{}
}

func TestExistingDependenciesAreNotReportedMissing(t *testing.T) {
	workspace := readyWorkspaceWithMissingDependencies()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: workspace.Status.RuntimePodName, Namespace: testNamespace}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: workspace.Status.HomeVolumeClaimName, Namespace: testNamespace}}
	report, err := Scan(t.Context(), fixtureClient(t, workspace, pod, pvc), testNamespace, DefaultPolicy(), auditNow)
	require.NoError(t, err)
	for _, finding := range report.Findings {
		assert.NotContains(t, []string{missingRuntimeCode, missingStorageCode, "RuntimeUnknown", "StorageUnknown"}, finding.Code)
	}
}

func TestMissingCoverageNeverMeansMissingResource(t *testing.T) {
	ref := Reference{Target: ObjectRef{APIVersion: "v1", Kind: podKind, Namespace: testNamespace, Name: "runtime"}, Relation: runtimeRelation}
	inventory := Inventory{Namespace: testNamespace, Complete: true}
	assert.Equal(t, "RuntimeUnknown", unresolvedDependency(Resource{}, ref, inventory).Code)
	inventory.Coverage = []Observation{{APIVersion: "v1", Kind: podKind, Complete: true}}
	inventory.Namespace = "another"
	assert.Equal(t, "RuntimeUnknown", unresolvedDependency(Resource{}, ref, inventory).Code)
}
