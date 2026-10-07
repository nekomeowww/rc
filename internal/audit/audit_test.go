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

func TestWorktreeSafetyExcludesRecentExecLocksHoldersAndMounts(t *testing.T) {
	worktree := &repositories.Worktree{ObjectMeta: fixtureMeta(testWorktree)}
	for name, related := range map[string]client.Object{
		"workspace-mount": &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName), Spec: workspaces.WorkspaceSpec{Mounts: []workspaces.WorkspaceMount{{Name: testWorktree, WorktreeRef: &workspaces.LocalReference{Name: testWorktree}}}}},
		"active-exec":     &repositories.WorktreeExec{ObjectMeta: fixtureMeta("exec"), Spec: repositories.WorktreeExecSpec{WorktreeRef: repositories.WorktreeReference{Name: testWorktree}}},
		"recent-exec":     &repositories.WorktreeExec{ObjectMeta: fixtureMeta("exec"), Spec: repositories.WorktreeExecSpec{WorktreeRef: repositories.WorktreeReference{Name: testWorktree}}, Status: repositories.WorktreeExecStatus{CompletedAt: new(metav1.NewTime(auditNow)), Conditions: []metav1.Condition{{Type: repositories.WorktreeExecConditionSucceeded, Status: metav1.ConditionFalse, LastTransitionTime: metav1.NewTime(auditNow)}}}},
	} {
		t.Run(name, func(t *testing.T) {
			report := scanReport(t, fixtureClient(t, worktree.DeepCopy(), related))
			for _, finding := range report.Findings {
				assert.NotEqual(t, "UnreferencedWorktree", finding.Code)
			}
		})
	}
	// The controller's published hold set and InUse condition replace Lease parsing.
	for name, status := range map[string]repositories.WorktreeStatus{
		"in-use":  {Conditions: []metav1.Condition{{Type: repositories.WorktreeConditionInUse, Status: metav1.ConditionTrue, Reason: repositories.WorktreeReasonPodConsumer}}},
		"used-by": {UsedBy: []repositories.UsageReference{{Kind: worktreeExecKind, Name: "writer", UID: "writer-uid", Mode: "write"}}},
	} {
		t.Run(name, func(t *testing.T) {
			held := worktree.DeepCopy()
			held.Status = status
			report := scanReport(t, fixtureClient(t, held))
			for _, finding := range report.Findings {
				assert.NotEqual(t, "UnreferencedWorktree", finding.Code)
			}
		})
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

func TestDoctorAttributesRepositoryHolders(t *testing.T) {
	workspace := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName)}
	repository := &repositories.Repository{ObjectMeta: fixtureMeta("source")}
	deleting := metav1.NewTime(auditNow.Add(-time.Minute))
	repository.DeletionTimestamp = &deleting
	repository.Finalizers = []string{"example.test/hold"}
	repository.Status.Access = &repositories.RepositoryAccessStatus{Mode: "mount", Holders: []repositories.UsageReference{{Kind: workspaceKind, Name: workspace.Name, UID: workspace.UID, Mode: "mount"}}}
	report := scanReport(t, fixtureClient(t, workspace, repository))
	finding := requireFinding(t, report, "DeletionPending")
	assert.Contains(t, finding.Message, "holders=Workspace/dev (mount)")
	assert.Contains(t, finding.Related, ObjectRef{APIVersion: workspaceAPI, Kind: workspaceKind, Namespace: testNamespace, Name: workspace.Name, UID: workspace.UID})
	g := newGraph(report.Inventory)
	root, ok := g.resolve(ObjectRef{APIVersion: workspaceAPI, Kind: workspaceKind, Namespace: testNamespace, Name: testWorkspaceName})
	require.True(t, ok)
	attributed := false
	for _, incoming := range g.incoming(root) {
		attributed = attributed || (incoming.Target.Kind == "Repository" && incoming.Relation == "referenced-by:"+holderRelation)
	}
	assert.True(t, attributed, "the Repository's published holder links to its Workspace")
}
