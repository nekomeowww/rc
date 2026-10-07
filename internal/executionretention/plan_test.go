package executionretention

import (
	"testing"
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPolicyForAppliesCountPolicy(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	completed := metav1.NewTime(now.Add(-time.Hour))
	executions := []workspacesv1alpha1.WorkspaceExec{
		{ObjectMeta: metav1.ObjectMeta{Name: "new"}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, CompletedAt: &completed}},
		{ObjectMeta: metav1.ObjectMeta{Name: "old"}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, CompletedAt: &completed}},
	}
	target := &workspacesv1alpha1.Workspace{Spec: workspacesv1alpha1.WorkspaceSpec{ExecutionRetention: &workspacesv1alpha1.ExecutionRetentionPolicy{MaxEntries: 1, TTLAfterFinished: &metav1.Duration{Duration: 24 * time.Hour}}}}
	policy, ok := PolicyFor(target)
	require.True(t, ok)
	plan := Build(executions, policy, now)
	require.Equal(t, []int{1}, plan.Remove)
}
