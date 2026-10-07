package worktrees

import (
	"context"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

const (
	ownershipWorkspaceName = "dev"
	ownershipWorktreeName  = "code"
	ownershipNamespace     = "test"
	ownershipWorkspaceUID  = "owner-uid"
	ownershipWorktreeUID   = "code-uid"
)

func TestWorktreeOwnershipTransitionsPreserveCheckout(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorkspaceName, Namespace: ownershipNamespace, UID: ownershipWorkspaceUID}}
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorktreeName, Namespace: ownershipNamespace, UID: ownershipWorktreeUID}, Spec: repositoriesv1alpha1.WorktreeSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: "repo"}, Branch: "feature"}, Status: repositoriesv1alpha1.WorktreeStatus{VolumeClaimName: "code-volume", WorktreePath: "/repository", Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue}}}}
	worktreeownership.InitializeGenerated(workspace, worktree)
	original := worktree.DeepCopy()
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "code-volume", Namespace: ownershipNamespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(worktree, repositoriesv1alpha1.SchemeGroupVersion.WithKind("Worktree"))}}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, worktree, claim).Build()
	require.NoError(t, changeWorktreeOwnership(ctx, kube, ownershipNamespace, ownershipWorktreeName, ownershipWorkspaceName, false))
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	assert.Empty(t, worktree.OwnerReferences)
	assert.Empty(t, worktree.Labels[worktreeownership.GeneratedForLabel])
	assert.Equal(t, original.Spec, worktree.Spec)
	assert.Equal(t, original.Status, worktree.Status)
	require.NoError(t, changeWorktreeOwnership(ctx, kube, ownershipNamespace, ownershipWorktreeName, ownershipWorkspaceName, true))
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	assert.True(t, worktreeownership.IsOwnedBy(worktree, workspace))
	assert.Empty(t, worktree.Labels[worktreeownership.GeneratedForLabel], "adopt does not change bootstrap mode")
	require.NoError(t, changeWorktreeOwnership(ctx, kube, ownershipNamespace, ownershipWorktreeName, ownershipWorkspaceName, true), "adoption is idempotent")
}

func TestWorktreeOwnershipRejectsUnsafeTransitions(t *testing.T) {
	for _, scenario := range []string{"deleting-owner", "deleting-worktree", "stale-owner-uid", "not-ready", "foreign-owner", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
			require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
			workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorkspaceName, Namespace: ownershipNamespace, UID: ownershipWorkspaceUID, Finalizers: []string{"test/hold"}}}
			worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorktreeName, Namespace: ownershipNamespace, UID: ownershipWorktreeUID}, Status: repositoriesv1alpha1.WorktreeStatus{Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue}}}}
			worktreeownership.InitializeGenerated(workspace, worktree)
			now := metav1.Now()
			adopt := false
			switch scenario {
			case "deleting-owner":
				workspace.DeletionTimestamp = &now
			case "deleting-worktree":
				worktree.DeletionTimestamp = &now
			case "stale-owner-uid":
				workspace.UID = "replacement"
			case "not-ready":
				worktree.Generation = 2
			case "foreign-owner":
				worktree.OwnerReferences[0].UID = "foreign"
				adopt = true
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, worktree).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if scenario == "conflict" {
					current := new(repositoriesv1alpha1.Worktree)
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
						return err
					}
					current.Labels["concurrent"] = "edit"
					if err := c.Update(ctx, current); err != nil {
						return err
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			}}).Build()
			require.Error(t, changeWorktreeOwnership(ctx, kube, ownershipNamespace, ownershipWorktreeName, ownershipWorkspaceName, adopt))
			current := new(repositoriesv1alpha1.Worktree)
			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), current))
			assert.Equal(t, worktree.OwnerReferences, current.OwnerReferences)
		})
	}
}

func TestWorktreeOwnershipCommandsRequireExplicitWorkspace(t *testing.T) {
	root := &cobra.Command{Use: "rcctl"}
	Register(root, kubeconfig.NewFlags())
	for _, verb := range []string{"adopt", "detach"} {
		cmd, _, err := root.Find([]string{"worktree", verb})
		require.NoError(t, err)
		require.Equal(t, verb, cmd.Name())
		require.ErrorContains(t, cmd.RunE(cmd, []string{ownershipWorktreeName}), "--workspace is required")
	}
}
