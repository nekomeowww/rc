package audit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func condition(kind string, status metav1.ConditionStatus, reason, message string, age time.Duration) metav1.Condition {
	return metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message, LastTransitionTime: metav1.NewTime(auditNow.Add(-age))}
}

func findings(report Report, code string) []Finding {
	result := []Finding{}
	for _, finding := range report.Findings {
		if finding.Code == code {
			result = append(result, finding)
		}
	}
	return result
}

func TestCompletionTimeIsReadFromStatus(t *testing.T) {
	succeeded := []metav1.Condition{condition(repositories.WorktreeExecConditionSucceeded, metav1.ConditionTrue, "Succeeded", "", time.Hour)}
	completed := metav1.NewTime(auditNow.Add(-2 * time.Hour))
	dated := &repositories.WorktreeExec{ObjectMeta: fixtureMeta("dated"), Status: repositories.WorktreeExecStatus{CompletedAt: &completed, Conditions: succeeded}}
	undated := &repositories.RepositoryExec{ObjectMeta: fixtureMeta("undated"), Status: repositories.RepositoryExecStatus{Conditions: succeeded}}
	report := scanReport(t, fixtureClient(t, dated, undated))
	byName := map[string]Resource{}
	for _, r := range report.Inventory.Resources {
		byName[r.Name] = r
	}
	require.NotNil(t, byName[dated.Name].CompletedAt)
	assert.True(t, completed.Equal(byName[dated.Name].CompletedAt), "completedAt, not the Succeeded transition time")
	assert.True(t, byName[undated.Name].Terminal)
	assert.Nil(t, byName[undated.Name].CompletedAt, "a missing completedAt stays undated rather than guessed")
}

func TestStorageNameIsNeverGuessedFromObjectName(t *testing.T) {
	// A PVC that happens to share the Workspace name must not be adopted as its
	// storage, and an unpublished name must not become MissingStorage.
	workspace := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName)}
	darwin := &workspaces.Workspace{ObjectMeta: fixtureMeta("mac"), Spec: workspaces.WorkspaceSpec{OS: "darwin"}}
	repository := &repositories.Repository{ObjectMeta: fixtureMeta("repo")}
	lookalike := &corev1.PersistentVolumeClaim{ObjectMeta: fixtureMeta(testWorkspaceName)}
	report := scanReport(t, fixtureClient(t, workspace, darwin, repository, lookalike))
	assert.Empty(t, findings(report, missingStorageCode))
	assert.Empty(t, findings(report, "PVCConflict"))
	unpublished := findings(report, "StorageUnpublished")
	names := make([]string, 0, len(unpublished))
	for _, finding := range unpublished {
		assert.Equal(t, severityUnknown, finding.Severity)
		names = append(names, finding.Resource.Name)
	}
	assert.ElementsMatch(t, []string{workspace.Name, repository.Name}, names, "Darwin Workspaces publish no PVC")
	for _, r := range report.Inventory.Resources {
		if r.Kind == workspaceKind {
			for _, ref := range r.References {
				assert.NotEqual(t, pvcRelation, ref.Relation)
			}
		}
	}
}

func TestPublishedStorageAndRuntimeReasonsBecomeFindings(t *testing.T) {
	lost := &workspaces.Workspace{ObjectMeta: fixtureMeta("lost"), Status: workspaces.WorkspaceStatus{HomeVolumeClaimName: "lost-home", RuntimePodName: "lost-runtime", Conditions: []metav1.Condition{
		condition(workspaces.ConditionStorageReady, metav1.ConditionFalse, workspaces.ReasonVolumeClaimLost, "home PVC is missing", time.Minute),
		condition(workspaces.WorkspaceConditionReady, metav1.ConditionFalse, workspaces.WorkspaceReasonRuntimeMissing, "replacing runtime Pod", time.Minute),
	}}}
	terminal := &workspaces.Workspace{ObjectMeta: fixtureMeta("terminal"), Status: workspaces.WorkspaceStatus{HomeVolumeClaimName: "terminal-home", RuntimePodName: "terminal-runtime", Conditions: []metav1.Condition{
		condition(workspaces.ConditionStorageReady, metav1.ConditionTrue, workspaces.ReasonVolumeClaimBound, "", time.Hour),
		condition(workspaces.WorkspaceConditionReady, metav1.ConditionFalse, workspaces.WorkspaceReasonRuntimeTerminal, "Evicted", time.Minute),
		condition(workspaces.WorkspaceConditionDegraded, metav1.ConditionTrue, "RuntimeFailed", "Evicted", time.Minute),
	}}}
	environment := &workspaces.WorkspaceEnvironment{ObjectMeta: fixtureMeta("env"), Status: workspaces.WorkspaceEnvironmentStatus{CurrentVolumeClaimName: "env-current", Conditions: []metav1.Condition{
		condition(workspaces.ConditionStorageReady, metav1.ConditionFalse, workspaces.ReasonVolumeClaimConflict, "PVC is controlled by another object", time.Minute),
	}}}
	objects := []client.Object{lost, terminal, environment,
		&corev1.PersistentVolumeClaim{ObjectMeta: fixtureMeta("terminal-home")},
		&corev1.Pod{ObjectMeta: fixtureMeta("terminal-runtime"), Status: corev1.PodStatus{Phase: corev1.PodFailed}},
		&corev1.PersistentVolumeClaim{ObjectMeta: fixtureMeta("env-current")},
	}
	report := scanReport(t, fixtureClient(t, objects...))

	missing := findings(report, missingStorageCode)
	require.Len(t, missing, 1, "the published reason and the existence check report once")
	assert.Equal(t, lost.Name, missing[0].Resource.Name)
	assert.Contains(t, missing[0].Message, "home PVC is missing")
	runtime := findings(report, missingRuntimeCode)
	require.Len(t, runtime, 1)
	assert.Contains(t, runtime[0].Message, workspaces.WorkspaceReasonRuntimeMissing)

	terminated := requireFinding(t, report, "TerminalRuntimePod")
	assert.Equal(t, terminal.Name, terminated.Resource.Name)
	assert.Contains(t, terminated.Message, "RuntimeFailed")
	require.Len(t, terminated.Related, 1)
	assert.Equal(t, "terminal-runtime", terminated.Related[0].Name)

	conflict := requireFinding(t, report, "PVCConflict")
	assert.Equal(t, environment.Name, conflict.Resource.Name)
	assert.Equal(t, "warning", conflict.Severity)
}

func TestReadyStatusIsTrustedForOwnershipButNotExistence(t *testing.T) {
	// The controller judges ownership; doctor only checks that what status names exists.
	workspace := &workspaces.Workspace{ObjectMeta: fixtureMeta(testWorkspaceName), Status: workspaces.WorkspaceStatus{HomeVolumeClaimName: "home", RuntimePodName: "runtime", Conditions: []metav1.Condition{
		condition(workspaces.ConditionStorageReady, metav1.ConditionTrue, workspaces.ReasonVolumeClaimBound, "", time.Hour),
		condition(workspaces.WorkspaceConditionReady, metav1.ConditionTrue, "Ready", "", time.Hour),
	}}}
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: fixtureMeta("home")}
	report := scanReport(t, fixtureClient(t, workspace, foreign))
	assert.Empty(t, findings(report, "PVCConflict"))
	assert.Empty(t, findings(report, missingStorageCode))
	requireFinding(t, report, missingRuntimeCode)
}

func TestDeletionFindingsUseDeletionBlocked(t *testing.T) {
	deleting := func(meta metav1.ObjectMeta, deletingFor time.Duration) metav1.ObjectMeta {
		meta.Finalizers, meta.DeletionTimestamp = []string{"workspaces.rc.ayaka.io/cleanup"}, new(metav1.NewTime(auditNow.Add(-deletingFor)))
		return meta
	}
	blocked := &workspaces.Workspace{ObjectMeta: deleting(fixtureMeta("blocked"), 2*time.Hour), Status: workspaces.WorkspaceStatus{Conditions: []metav1.Condition{
		condition(workspaces.WorkspaceConditionDeletionBlocked, metav1.ConditionTrue, workspaces.WorkspaceReasonWaitingForExecutions, "WorkspaceExec build is running", time.Hour),
	}}}
	recent := &repositories.Worktree{ObjectMeta: deleting(fixtureMeta("recent"), 2*time.Hour), Status: repositories.WorktreeStatus{Conditions: []metav1.Condition{
		condition(repositories.WorktreeConditionDeletionBlocked, metav1.ConditionTrue, repositories.DeletionBlockedReasonWaitingForPods, "Pod helper uses the volume", time.Minute),
	}}}
	unpublished := &workspaces.Workspace{ObjectMeta: deleting(fixtureMeta("unpublished"), 2*time.Hour)}
	execution := terminalRecord("exec")
	execution.DeletionTimestamp = new(metav1.NewTime(auditNow.Add(-2 * time.Hour)))
	report := scanReport(t, fixtureClient(t, blocked, recent, unpublished, execution))
	byName := map[string]Finding{}
	for _, finding := range append(findings(report, "DeletionBlocker"), findings(report, "DeletionPending")...) {
		byName[finding.Resource.Name] = finding
	}

	assert.Equal(t, "DeletionBlocker", byName[blocked.Name].Code)
	assert.Equal(t, "DeletionBlocked=WaitingForExecutions for 1h0m0s: WorkspaceExec build is running", byName[blocked.Name].Message)
	assert.Equal(t, "DeletionPending", byName[recent.Name].Code, "age counts from the blocker's transition, not deletion start")
	assert.Contains(t, byName[recent.Name].Message, repositories.DeletionBlockedReasonWaitingForPods)
	assert.Equal(t, "DeletionBlocker", byName[unpublished.Name].Code)
	assert.Contains(t, byName[unpublished.Name].Message, "DeletionBlocked is not published")
	assert.Contains(t, byName[execution.Name].Message, "finalizers="+workspaceExecFinalizer, "kinds without DeletionBlocked fall back to finalizers")
	assert.NotContains(t, byName[execution.Name].Message, "DeletionBlocked")
}
