package workspaces

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func TestApplyDeferredWorktreeMount(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name             string
		owner            string
		readOnly         bool
		volumeGeneration int64
		wantError        bool
	}{
		{name: "initializes in owner Workspace", owner: testWorkspaceName, volumeGeneration: 2},
		{name: "rejects another Workspace", owner: "foreign-workspace", volumeGeneration: 2, wantError: true},
		{name: "rejects read-only initialization", owner: testWorkspaceName, readOnly: true, volumeGeneration: 2, wantError: true},
		{name: "rejects stale volume readiness", owner: testWorkspaceName, volumeGeneration: 1, wantError: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			scheme := runtime.NewScheme()
			require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
			require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
			require.NoError(t, coordinationv1.AddToScheme(scheme))
			workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace, UID: testWorkspaceUID}}
			worktree := &repositoriesv1alpha1.Worktree{
				ObjectMeta: metav1.ObjectMeta{Name: "deferred", Namespace: testWorkspaceNamespace, UID: testWorktreeUID, Generation: 2,
					Labels: map[string]string{"workspaces.rc.ayaka.io/generated-for": scenario.owner}},
				Spec: repositoriesv1alpha1.WorktreeSpec{Branch: "feature"},
				Status: repositoriesv1alpha1.WorktreeStatus{ObservedGeneration: 2, VolumeClaimName: "child", Conditions: []metav1.Condition{{
					Type: repositoriesv1alpha1.WorktreeConditionVolumeReady, Status: metav1.ConditionTrue, ObservedGeneration: scenario.volumeGeneration, Reason: "VolumeReady",
				}}},
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, worktree).Build()
			stopCalls := 0
			_, err := applyWorktreeMount(context.Background(), kubeClient, workspace, workspace.Namespace, worktree.Name, mountOptions{readOnly: scenario.readOnly},
				func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) { stopCalls++; return nil, nil })
			if scenario.wantError {
				require.Error(t, err)
				require.Zero(t, stopCalls)
				return
			}
			// Ready depends on mounting this clone in the consuming Workspace.
			// Requiring Ready here prevents the initializer from ever running.
			require.NoError(t, err)
			require.Equal(t, 1, stopCalls)
		})
	}
}
