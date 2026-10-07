package workspaces

import (
	"fmt"
	"testing"
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/executionretention"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExecutionRetentionProtectsActiveAndPinnedAndUsesCompletion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	old := metav1.NewTime(now.Add(-120 * 24 * time.Hour))
	recent := metav1.NewTime(now.Add(-time.Hour))
	executions := make([]workspacesv1alpha1.WorkspaceExec, 0, 11)
	for _, phase := range []workspacesv1alpha1.WorkspaceExecPhase{"", workspacesv1alpha1.WorkspaceExecPhasePending, workspacesv1alpha1.WorkspaceExecPhaseStarting, workspacesv1alpha1.WorkspaceExecPhaseRunning, workspacesv1alpha1.WorkspaceExecPhaseSucceeded, workspacesv1alpha1.WorkspaceExecPhaseFailed, workspacesv1alpha1.WorkspaceExecPhaseStopped, workspacesv1alpha1.WorkspaceExecPhaseLost} {
		executions = append(executions, workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: string(phase), CreationTimestamp: old}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: phase, CompletedAt: &old}})
	}
	executions = append(executions,
		workspacesv1alpha1.WorkspaceExec{Spec: workspacesv1alpha1.WorkspaceExecSpec{Retain: true}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseFailed, CompletedAt: &old}},
		workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: old}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, CompletedAt: &recent}},
		workspacesv1alpha1.WorkspaceExec{Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseLost}},
	)
	plan := executionretention.Build(executions, &workspacesv1alpha1.ExecutionRetentionPolicy{}, now)
	require.ElementsMatch(t, []int{4, 5, 6, 7}, plan.Remove)
	require.ElementsMatch(t, []int{0, 1, 2, 3, 8, 9, 10}, plan.Keep)
	require.Len(t, executionretention.Build(executions, nil, now).Keep, len(executions), "omission is upgrade-compatible")
}

func TestExecutionRetentionCombinesCountTTLAndTranscriptTTLDeterministically(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	executions := make([]workspacesv1alpha1.WorkspaceExec, 5)
	for i := range executions {
		completed := metav1.NewTime(now.Add(-time.Duration(i+1) * time.Hour))
		executions[i] = workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprint(i)}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseFailed, CompletedAt: &completed}}
	}
	policy := &workspacesv1alpha1.ExecutionRetentionPolicy{MaxEntries: 3, TTLAfterFinished: &metav1.Duration{Duration: 3 * time.Hour}, TranscriptTTL: &metav1.Duration{Duration: 2 * time.Hour}}
	plan := executionretention.Build(executions, policy, now)
	require.Equal(t, []int{4, 3, 2}, plan.Remove, "TTL wins at its exact boundary; oldest removal first")
	require.False(t, transcriptExpired(&executions[0], policy, now))
	require.True(t, transcriptExpired(&executions[1], policy, now), "storage TTL applies independently of CR removal")
	policy.TTLAfterFinished.Duration = 24 * time.Hour
	require.Equal(t, []int{4, 3}, executionretention.Build(executions, policy, now).Remove, "count wins before TTL")
	executions[4].Status.CompletedAt = executions[3].Status.CompletedAt
	require.Equal(t, []int{4, 3}, executionretention.Build(executions, policy, now).Remove, "name breaks equal-time ties")
}

func TestTranscriptDefaultAndPin(t *testing.T) {
	t.Parallel()
	now := time.Now()
	done := metav1.NewTime(now.Add(-executionretention.DefaultTranscriptTTL))
	process := &workspacesv1alpha1.WorkspaceExec{Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, CompletedAt: &done}}
	require.False(t, transcriptExpired(process, nil, now))
	require.False(t, transcriptExpired(process, &workspacesv1alpha1.ExecutionRetentionPolicy{}, now.Add(-time.Nanosecond)))
	require.True(t, transcriptExpired(process, &workspacesv1alpha1.ExecutionRetentionPolicy{}, now))
	process.Spec.Retain = true
	require.False(t, transcriptExpired(process, &workspacesv1alpha1.ExecutionRetentionPolicy{}, now))
}

func TestExecutionRetentionTemplateBounds(t *testing.T) {
	t.Parallel()
	now := time.Now()
	recent := metav1.NewTime(now.Add(-time.Hour))
	records := make([]workspacesv1alpha1.WorkspaceExec, executionretention.DefaultMaxEntries+1)
	for i := range records {
		records[i] = workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("exec-%05d", i)}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, CompletedAt: &recent}}
	}
	policy := &workspacesv1alpha1.ExecutionRetentionPolicy{}
	require.Equal(t, []int{executionretention.DefaultMaxEntries}, executionretention.Build(records, policy, now).Remove)
	records[0].Spec.Retain = true
	require.Empty(t, executionretention.Build(records, policy, now).Remove, "pins do not consume the 15000-record budget")
	boundary := metav1.NewTime(now.Add(-executionretention.DefaultTTL))
	records[1].Status.CompletedAt = &boundary
	require.Equal(t, []int{1}, executionretention.Build(records, policy, now).Remove, "90-day TTL applies at the exact completion boundary")
}
