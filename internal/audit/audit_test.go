package audit

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

const testWorktree = "code"
const testWorkspaceName = "dev"
const testNamespace = "audit"

var auditNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func fixtureClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{repositories.AddToScheme, workspaces.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme, coordinationv1.AddToScheme} {
		require.NoError(t, add(scheme))
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func fixtureMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: types.UID(name + "-uid"), CreationTimestamp: metav1.NewTime(auditNow.Add(-30 * 24 * time.Hour))}
}

func terminalRecord(name string) *workspaces.WorkspaceExec {
	finished := metav1.NewTime(auditNow.Add(-14 * 24 * time.Hour))
	metadata := fixtureMeta(name)
	controller := true
	metadata.Finalizers = []string{workspaceExecFinalizer}
	metadata.OwnerReferences = []metav1.OwnerReference{{APIVersion: workspaceAPI, Kind: workspaceKind, Name: testWorkspaceName, UID: "dev-uid", Controller: &controller}}
	return &workspaces.WorkspaceExec{ObjectMeta: metadata, Spec: workspaces.WorkspaceExecSpec{TargetRef: workspaces.WorkspaceExecTargetReference{Kind: workspaces.WorkspaceExecTargetWorkspace, Name: testWorkspaceName}}, Status: workspaces.WorkspaceExecStatus{Phase: workspaces.WorkspaceExecPhaseSucceeded, CompletedAt: &finished}}
}

func scanReport(t *testing.T, kube client.Client) Report {
	t.Helper()
	report, err := Scan(t.Context(), kube, testNamespace, DefaultPolicy(), auditNow)
	require.NoError(t, err)
	return report
}

func TestPartialVisibilityIsUnknown(t *testing.T) {
	base := fixtureClient(t, terminalRecord("done"), &repositories.Worktree{ObjectMeta: fixtureMeta("idle")})
	kube := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*workspaces.WorkspaceList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "workspaces"}, "", assert.AnError)
		}
		return c.List(ctx, list, opts...)
	}})
	report := scanReport(t, kube)
	assert.False(t, report.Inventory.Complete)
	requireFinding(t, report, "VisibilityUnknown")
	for _, finding := range report.Findings {
		assert.NotEqual(t, "UnreferencedWorktree", finding.Code)
	}
	history, err := ExecutionHistory(t.Context(), kube, testNamespace, auditNow)
	require.NoError(t, err)
	assert.False(t, history.Complete)
	assert.Empty(t, history.Targets)
}

func TestWorktreeSafetyExcludesRecentExecLocksLeasesAndMounts(t *testing.T) {
	worktree := &repositories.Worktree{ObjectMeta: fixtureMeta(testWorktree)}
	for name, related := range map[string]client.Object{
		"mount":       &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName), Spec: workspaces.WorkspaceSpec{Mounts: []workspaces.WorkspaceMount{{Name: testWorktree, WorktreeRef: &workspaces.LocalReference{Name: testWorktree}}}}},
		"active-exec": &repositories.WorktreeExec{ObjectMeta: fixtureMeta("exec"), Spec: repositories.WorktreeExecSpec{WorktreeRef: repositories.WorktreeReference{Name: testWorktree}}},
		"recent-exec": &repositories.WorktreeExec{ObjectMeta: fixtureMeta("exec"), Spec: repositories.WorktreeExecSpec{WorktreeRef: repositories.WorktreeReference{Name: testWorktree}}, Status: repositories.WorktreeExecStatus{CompletedAt: new(metav1.NewTime(auditNow)), Conditions: []metav1.Condition{{Type: repositories.WorktreeExecConditionSucceeded, Status: metav1.ConditionFalse, LastTransitionTime: metav1.NewTime(auditNow)}}}},
		"lease":       &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: worktreeownership.LegacyWriteLeaseName(worktree), Namespace: worktree.Namespace}},
	} {
		t.Run(name, func(t *testing.T) {
			report := scanReport(t, fixtureClient(t, worktree.DeepCopy(), related))
			for _, finding := range report.Findings {
				assert.NotEqual(t, "UnreferencedWorktree", finding.Code)
			}
		})
	}
	worktree.Spec.Lock = true
	report := scanReport(t, fixtureClient(t, worktree))
	for _, finding := range report.Findings {
		assert.NotEqual(t, "UnreferencedWorktree", finding.Code)
	}
}

func TestStaleProvisioningEventCannotDiagnoseReplacementPVC(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: fixtureMeta("pending"), Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}}
	event := &corev1.Event{ObjectMeta: fixtureMeta("event"), Reason: "ProvisioningFailed", InvolvedObject: corev1.ObjectReference{APIVersion: "v1", Kind: pvcKind, Namespace: testNamespace, Name: pvc.Name, UID: "previous-uid"}}
	report := scanReport(t, fixtureClient(t, pvc, event))
	for _, finding := range report.Findings {
		assert.NotEqual(t, "ProvisioningFailure", finding.Code)
	}
}

func TestLongUnhealthyAndLargeStorageAreEvidenceNotDeletePermission(t *testing.T) {
	worktree := &repositories.Worktree{ObjectMeta: fixtureMeta(testWorktree), Status: repositories.WorktreeStatus{Conditions: []metav1.Condition{{Type: repositories.WorktreeConditionReady, Status: metav1.ConditionFalse, Reason: "BootstrapFailed", LastTransitionTime: metav1.NewTime(auditNow.Add(-time.Hour))}}}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: fixtureMeta(testWorktree), Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("200Gi")}}}}
	report := scanReport(t, fixtureClient(t, worktree, pvc))
	codes := make([]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		codes = append(codes, finding.Code)
	}
	assert.Contains(t, codes, "LongUnhealthy")
	assert.Contains(t, codes, "LargeStorageRequest")
}

func TestDataResourcesStayInDoctorOnly(t *testing.T) {
	workspace := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName), Spec: workspaces.WorkspaceSpec{OS: "darwin"}}
	worktree := &repositories.Worktree{ObjectMeta: fixtureMeta(testWorktree), Status: repositories.WorktreeStatus{VolumeClaimName: testWorktree, Conditions: []metav1.Condition{
		{Type: repositories.WorktreeConditionVolumeReady, Status: metav1.ConditionFalse, Reason: repositories.WorktreeReasonVolumeClaimConflict, Message: "PVC is controlled by another object", LastTransitionTime: metav1.NewTime(auditNow)},
	}}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: fixtureMeta(testWorktree), Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")}}}}
	kube := fixtureClient(t, workspace, worktree, pvc)
	report := scanReport(t, kube)
	conflict := requireFinding(t, report, "PVCConflict")
	assert.Equal(t, worktree.Name, conflict.Resource.Name)
	assert.Contains(t, conflict.Message, repositories.WorktreeReasonVolumeClaimConflict)
	require.Len(t, conflict.Related, 1)
	assert.Equal(t, pvc.Name, conflict.Related[0].Name)
	assert.EqualValues(t, 40*1024*1024*1024, report.Summary.PVCRequestedBytes)
}

func TestRepositoryReservationAttributesHolderByRoleToken(t *testing.T) {
	workspace := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName)}
	reserved := &coordinationv1.Lease{ObjectMeta: fixtureMeta("rc-repository-reserved")}
	// Gate tokens use the consumer role ("workspace"), not the Kind ("Workspace").
	reserved.Annotations = map[string]string{repositoryaccess.StateAnnotation: `{"mode":"mount","holders":{"workspace/` + string(workspace.UID) + "/" + workspace.Name + `":true}}`}
	malformed := &coordinationv1.Lease{ObjectMeta: fixtureMeta("rc-repository-malformed")}
	malformed.Annotations = map[string]string{repositoryaccess.StateAnnotation: "{"}
	unrelated := &coordinationv1.Lease{ObjectMeta: fixtureMeta("rc-repository-unrelated")}
	unrelated.Annotations = map[string]string{repositoryaccess.StateAnnotation: `{"mode":"mount","holders":{"workspace/other-uid/other":true}}`}
	g := newGraph(scanReport(t, fixtureClient(t, workspace, reserved, malformed, unrelated)).Inventory)
	root, ok := g.resolve(ObjectRef{APIVersion: workspaceAPI, Kind: workspaceKind, Namespace: testNamespace, Name: testWorkspaceName})
	require.True(t, ok)
	leases := g.leases(root)
	names := make([]string, 0, len(leases))
	for _, lease := range leases {
		names = append(names, lease.Name)
	}
	assert.ElementsMatch(t, []string{reserved.Name, malformed.Name}, names, "undecodable reservations fail closed as references")
}
