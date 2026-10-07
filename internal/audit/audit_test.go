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
	"github.com/nekomeowww/rc/internal/executionretention"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
)

const testWorktree = "code"
const testWorkspaceName = "dev"
const testNamespace = "audit"
const unrelatedCase = "unrelated"
const targetRBACCase = "target-rbac"
const targetPolicyCase = "target-policy"
const replacementCase = "replacement"
const absentCase = "absent"

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

// allowHistory is a test adapter, not an implementation of retention policy.
func allowHistory(records []workspaces.WorkspaceExec, target client.Object, _ time.Time) (executionretention.Plan, error) {
	plan := executionretention.Plan{}
	for i := range records {
		if !records[i].Spec.Retain && records[i].GetAnnotations()["test/deny"] == "" && (target == nil || target.GetAnnotations()["test/deny"] == "") {
			plan.Remove = append(plan.Remove, i)
		} else {
			plan.Keep = append(plan.Keep, i)
		}
	}
	return plan, nil
}

func reviewPlan(t *testing.T, kube client.Client) ReviewedPlan {
	t.Helper()
	target := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName)}
	if err := kube.Create(t.Context(), target); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	review, err := Review(t.Context(), kube, testNamespace, HistoryPolicy{HistoryFor: DefaultPolicy().HistoryFor}, nil, allowHistory, auditNow)
	require.NoError(t, err)
	return review
}

func scanReport(t *testing.T, kube client.Client) Report {
	t.Helper()
	report, err := Scan(t.Context(), kube, testNamespace, DefaultPolicy(), auditNow)
	require.NoError(t, err)
	return report
}

func TestPartialVisibilityIsUnknownAndBlocksPrune(t *testing.T) {
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
	review := reviewPlan(t, kube)
	assert.Empty(t, review.Plan().Candidates)
	_, err := Prune(t.Context(), kube, review, auditNow)
	require.ErrorContains(t, err, "unknown")
}

func TestWorktreeSafetyExcludesRecentExecLocksLeasesAndMounts(t *testing.T) {
	worktree := &repositories.Worktree{ObjectMeta: fixtureMeta(testWorktree)}
	for name, related := range map[string]client.Object{
		"mount":       &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName), Spec: workspaces.WorkspaceSpec{Mounts: []workspaces.WorkspaceMount{{Name: testWorktree, WorktreeRef: &workspaces.LocalReference{Name: testWorktree}}}}},
		"active-exec": &repositories.WorktreeExec{ObjectMeta: fixtureMeta("exec"), Spec: repositories.WorktreeExecSpec{WorktreeRef: repositories.WorktreeReference{Name: testWorktree}}},
		"recent-exec": &repositories.WorktreeExec{ObjectMeta: fixtureMeta("exec"), Spec: repositories.WorktreeExecSpec{WorktreeRef: repositories.WorktreeReference{Name: testWorktree}}, Status: repositories.WorktreeExecStatus{Conditions: []metav1.Condition{{Type: repositories.WorktreeExecConditionSucceeded, Status: metav1.ConditionFalse, LastTransitionTime: metav1.NewTime(auditNow)}}}},
		"lease":       worktreeclaim.DeletionLease(worktree),
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
	workspace := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName)}
	worktree := &repositories.Worktree{ObjectMeta: fixtureMeta(testWorktree)}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: fixtureMeta(testWorktree), Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")}}}}
	kube := fixtureClient(t, workspace, worktree, pvc)
	report := scanReport(t, kube)
	requireFinding(t, report, "PVCConflict")
	assert.EqualValues(t, 40*1024*1024*1024, report.Summary.PVCRequestedBytes)
	assert.Empty(t, reviewPlan(t, kube).Plan().Candidates)
}

func TestReviewRequiresCanonicalPolicyAndOnlyTightensAge(t *testing.T) {
	kube := fixtureClient(t, terminalRecord("done"), &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName)})
	for _, tc := range []struct {
		name     string
		age      time.Duration
		evaluate HistoryEvaluator
		eligible int
		unknown  bool
	}{
		{"not-integrated", time.Hour, nil, 0, true},
		{"canonical-retain", time.Hour, func(records []workspaces.WorkspaceExec, _ client.Object, _ time.Time) (executionretention.Plan, error) {
			return executionretention.Plan{Keep: []int{0}}, nil
		}, 0, false},
		{"canonical-permit", time.Hour, allowHistory, 1, false},
		{"stricter-cli-age", 30 * 24 * time.Hour, allowHistory, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review, err := Review(t.Context(), kube, testNamespace, HistoryPolicy{HistoryFor: tc.age}, nil, tc.evaluate, auditNow)
			require.NoError(t, err)
			assert.Len(t, review.Plan().Candidates, tc.eligible)
			assert.Equal(t, tc.unknown, len(review.Unknowns) > 0)
		})
	}
}

func TestReviewUsesTargetScopedCountPolicy(t *testing.T) {
	newer := terminalRecord("newer")
	older := terminalRecord("older")
	newer.Status.CompletedAt = new(metav1.NewTime(auditNow.Add(-8 * 24 * time.Hour)))
	older.Status.CompletedAt = new(metav1.NewTime(auditNow.Add(-9 * 24 * time.Hour)))
	target := &workspaces.Workspace{
		ObjectMeta: fixtureMeta(testWorkspaceName),
		Spec: workspaces.WorkspaceSpec{ExecutionRetention: &workspaces.ExecutionRetentionPolicy{
			TTLAfterFinished: &metav1.Duration{Duration: 365 * 24 * time.Hour},
			MaxEntries:       1,
		}},
	}
	review, err := Review(t.Context(), fixtureClient(t, newer, older, target), testNamespace, HistoryPolicy{HistoryFor: 7 * 24 * time.Hour}, nil, executionretention.BuildForTarget, auditNow)
	require.NoError(t, err)
	require.Len(t, review.Plan().Candidates, 1)
	assert.Equal(t, older.Name, review.Plan().Candidates[0].Name)
}

func TestCanonicalReviewFailsClosedWithoutTarget(t *testing.T) {
	_, err := Review(t.Context(), fixtureClient(t, terminalRecord("orphan")), testNamespace, HistoryPolicy{HistoryFor: 7 * 24 * time.Hour}, nil, executionretention.BuildForTarget, auditNow)
	require.ErrorContains(t, err, "target is absent")
}

func TestCanonicalReviewRetainsUnprovenOwnerIdentity(t *testing.T) {
	for _, mutate := range []func(*workspaces.WorkspaceExec){
		func(record *workspaces.WorkspaceExec) { record.OwnerReferences = nil },
		func(record *workspaces.WorkspaceExec) { record.OwnerReferences[0].UID = "" },
		func(record *workspaces.WorkspaceExec) { record.OwnerReferences[0].UID = "previous-target" },
	} {
		record := terminalRecord("unproven")
		mutate(record)
		target := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName), Spec: workspaces.WorkspaceSpec{ExecutionRetention: &workspaces.ExecutionRetentionPolicy{}}}
		review, err := Review(t.Context(), fixtureClient(t, record, target), testNamespace, HistoryPolicy{HistoryFor: 7 * 24 * time.Hour}, nil, executionretention.BuildForTarget, auditNow)
		require.NoError(t, err)
		assert.Empty(t, review.Plan().Candidates)
	}
}

func TestHistorySafetyBeforeAndAfterConfirmation(t *testing.T) {
	// ROOT CAUSE: an age-only plan ignored policy, and its full snapshot check
	// unnecessarily rejected unrelated objects. Reevaluate only the selected
	// record and direct target after review; canonical denials remain authoritative.
	for _, change := range []string{"active", "undated", "recent", "finalizer", "attachment", "retain", "owner", targetPolicyCase, targetRBACCase, replacementCase} {
		t.Run(change, func(t *testing.T) {
			record := terminalRecord("done")
			target := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName)}
			kube := fixtureClient(t, record, target)
			review := reviewPlan(t, kube)
			require.Len(t, review.Plan().Candidates, 1)
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(record), record))
			switch change {
			case "active":
				record.Status.Phase = workspaces.WorkspaceExecPhaseRunning
			case "undated":
				record.Status.CompletedAt = nil
			case "recent":
				record.Status.CompletedAt = new(metav1.NewTime(auditNow))
			case "finalizer":
				record.Finalizers = []string{"test/cleanup"}
			case "attachment":
				record.Status.AttachedClients = 1
			case "retain":
				record.Annotations = map[string]string{"test/deny": "canonical-decision"}
			case "owner":
				record.OwnerReferences = nil
			case replacementCase:
				record.UID = replacementCase
			case targetPolicyCase:
				require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(target), target))
				target.Annotations = map[string]string{"test/deny": targetPolicyCase}
				require.NoError(t, kube.Update(t.Context(), target))
			case targetRBACCase:
				kube = interceptor.NewClient(kube, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*workspaces.Workspace); ok {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "workspaces"}, k.Name, assert.AnError)
					}
					return c.Get(ctx, k, obj, opts...)
				}})
			}
			if change != targetPolicyCase && change != targetRBACCase {
				require.NoError(t, kube.Update(t.Context(), record))
			}
			result, err := Prune(t.Context(), kube, review, auditNow)
			require.Error(t, err)
			assert.Empty(t, result.Requested)
			if change == replacementCase || change == targetRBACCase {
				return
			}
			fresh := reviewPlan(t, kube)
			assert.Empty(t, fresh.Plan().Candidates, "fresh planning applies the same safety contract")
		})
	}
}

func TestSavedSelectionRevalidation(t *testing.T) {
	for _, change := range []string{unrelatedCase, "reference", "changed", absentCase, "duplicate", "data-kind", "scope", "expired"} {
		t.Run(change, func(t *testing.T) {
			record := terminalRecord("done")
			kube := fixtureClient(t, record)
			saved := reviewPlan(t, kube).Plan()
			switch change {
			case unrelatedCase:
				require.NoError(t, kube.Create(t.Context(), terminalRecord("new-eligible")))
			case "reference":
				require.NoError(t, kube.Create(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: testNamespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: workspaceAPI, Kind: workspaceExecKind, Name: record.Name, UID: record.UID}}}}))
			case "changed":
				require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(record), record))
				record.Labels = map[string]string{"updated": "yes"}
				require.NoError(t, kube.Update(t.Context(), record))
			case absentCase:
				record.Finalizers = nil
				require.NoError(t, kube.Update(t.Context(), record))
				require.NoError(t, kube.Delete(t.Context(), record))
			case "duplicate":
				saved.Candidates = append(saved.Candidates, saved.Candidates[0])
			case "data-kind":
				saved.Candidates[0].Kind = workspaceKind
			case "scope":
				saved.Candidates[0].Namespace = "outside-scope"
			case "expired":
				saved.ExpiresAt = metav1.NewTime(auditNow)
			}
			review, err := Review(t.Context(), kube, testNamespace, saved.Policy, &saved, allowHistory, auditNow)
			if change != unrelatedCase && change != absentCase {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, review.Plan().Candidates, 1)
			assert.Equal(t, "done", review.Plan().Candidates[0].Name, "saved selection never expands")
		})
	}
}

func TestPartialPruneCanResumeWithoutRepeatingDeletes(t *testing.T) {
	base := fixtureClient(t, terminalRecord("first"), terminalRecord("second"))
	review := reviewPlan(t, base)
	kube := interceptor.NewClient(base, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if obj.GetName() == "second" {
			return assert.AnError
		}
		return c.Delete(ctx, obj, opts...)
	}})
	result, err := Prune(t.Context(), kube, review, auditNow)
	require.Error(t, err)
	assert.Len(t, result.Requested, 1)
	saved := review.Plan()
	review, err = Review(t.Context(), base, testNamespace, saved.Policy, &saved, allowHistory, auditNow)
	require.NoError(t, err)
	result, err = Prune(t.Context(), base, review, auditNow)
	require.NoError(t, err)
	assert.Len(t, result.Requested, 1)
	assert.Len(t, result.Pending, 2)
}

func TestReviewBlocksLeaseAndLeavesLegacyHistoryDiagnosticOnly(t *testing.T) {
	record := terminalRecord("reserved")
	lease := &coordinationv1.Lease{ObjectMeta: fixtureMeta("reservation"), Spec: coordinationv1.LeaseSpec{HolderIdentity: new(string(record.UID))}}
	assert.Empty(t, reviewPlan(t, fixtureClient(t, record, lease)).Plan().Candidates)
	syncRecord := &repositories.RepositorySync{ObjectMeta: fixtureMeta("sync"), Spec: repositories.RepositorySyncSpec{RepositoryRef: repositories.RepositoryReference{Name: "source"}}, Status: repositories.RepositorySyncStatus{JobName: "sync-job", CompletedAt: record.Status.CompletedAt, Conditions: []metav1.Condition{{Type: repositories.WorktreeExecConditionSucceeded, Status: metav1.ConditionTrue, LastTransitionTime: *record.Status.CompletedAt}}}}
	kube := fixtureClient(t, syncRecord)
	assert.Empty(t, reviewPlan(t, kube).Plan().Candidates)
	require.NoError(t, kube.Create(t.Context(), &batchv1.Job{ObjectMeta: fixtureMeta("sync-job")}))
	assert.Empty(t, reviewPlan(t, kube).Plan().Candidates)
}

func TestConfirmationCannotExtendPlanLifetime(t *testing.T) {
	kube := fixtureClient(t, terminalRecord("expired-after-prompt"))
	review := reviewPlan(t, kube)
	result, err := Prune(t.Context(), kube, review, auditNow.Add(time.Hour))
	require.ErrorContains(t, err, "expired")
	assert.Empty(t, result.Requested)
}
