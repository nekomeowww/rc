package workspaces

import (
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/executionretention"
)

// planExecutionRetention is the sole age/count policy for both target kinds.
// Incomplete timestamps are retained until reconciliation records completion.
func planExecutionRetention(executions []workspacesv1alpha1.WorkspaceExec, policy *workspacesv1alpha1.ExecutionRetentionPolicy, now time.Time) executionretention.Plan {
	return executionretention.Build(executions, policy, now)
}
