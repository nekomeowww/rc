package workspaces

import (
	"bytes"
	"context"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	workspaceservice "github.com/nekomeowww/rc/internal/workspaces"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

const (
	ownershipTestRepository  = "repo"
	ownershipTestNamespace   = "test"
	ownershipTestIndependent = "independent"
	ownershipRunRepository   = "run-repo"
	ownershipMountRepository = "mount-repo"
	ownershipMountExisting   = "mount-existing"
	ownershipRunWorktree     = "run-worktree"
)

// TestGeneratedWorktreeLifecycle covers each source once. The deprecated CLI
// flag has no lifecycle effect. Fake clients do not run GC: owner identity is
// asserted as the GC contract, separately from explicit CLI DELETE requests.
func TestGeneratedWorktreeLifecycle(t *testing.T) {
	for _, source := range []string{ownershipRunRepository, ownershipMountRepository, ownershipMountExisting, ownershipRunWorktree} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
			require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
			require.NoError(t, coordinationv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: ownershipTestNamespace, UID: testWorkspaceUID}}
			repo := &repositoriesv1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: ownershipTestRepository, Namespace: ownershipTestNamespace}, Status: repositoriesv1alpha1.RepositoryStatus{Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue}}}}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repo).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*workspacesv1alpha1.Workspace); ok {
					obj.SetUID(workspace.UID)
				}
				return c.Create(ctx, obj, opts...)
			}}).Build()
			var worktree *repositoriesv1alpha1.Worktree
			switch source {
			case ownershipRunRepository:
				target, err := (&workspaceservice.Runner{Client: kube}).Prepare(ctx, workspaceservice.RunRequest{Create: true, Namespace: workspace.Namespace, Name: workspace.Name, Repositories: []workspaceservice.MountRequest{{Name: repo.Name}}})
				require.NoError(t, err)
				workspace = target.Workspace
				worktree = new(repositoriesv1alpha1.Worktree)
				require.NoError(t, kube.Get(ctx, client.ObjectKey{Namespace: ownershipTestNamespace, Name: "dev-repo"}, worktree))
			case ownershipMountRepository:
				require.NoError(t, kube.Create(ctx, workspace))
				worktree = generatedWorkspaceWorktree(workspace, repo, repo.Name, nil)
				worktree.Status = repositoriesv1alpha1.WorktreeStatus{VolumeClaimName: worktree.Name, Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue}}}
				require.NoError(t, kube.Create(ctx, worktree))
				_, err := applyWorkspaceMount(ctx, kube, client.ObjectKeyFromObject(workspace), workspacesv1alpha1.WorkspaceMount{Name: ownershipTestRepository, Path: ownershipTestRepository, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}}, func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) { return nil, nil })
				require.NoError(t, err)
				require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
			case ownershipRunWorktree:
				worktree = &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: ownershipTestIndependent, Namespace: ownershipTestNamespace, UID: "independent-uid"}, Status: repositoriesv1alpha1.WorktreeStatus{Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue}}}}
				require.NoError(t, kube.Create(ctx, worktree))
				target, err := (&workspaceservice.Runner{Client: kube}).Prepare(ctx, workspaceservice.RunRequest{Create: true, Namespace: workspace.Namespace, Name: workspace.Name, Worktrees: []workspaceservice.MountRequest{{Name: worktree.Name}}})
				require.NoError(t, err)
				workspace = target.Workspace
				require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
			case ownershipMountExisting:
				require.NoError(t, kube.Create(ctx, workspace))
				worktree = &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: ownershipTestIndependent, Namespace: ownershipTestNamespace, UID: "independent-uid"}, Status: repositoriesv1alpha1.WorktreeStatus{VolumeClaimName: ownershipTestIndependent, Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue}}}}
				require.NoError(t, kube.Create(ctx, worktree))
				_, err := applyWorktreeMount(ctx, kube, workspace, ownershipTestNamespace, worktree.Name, mountOptions{readOnly: true}, func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) { return nil, nil })
				require.NoError(t, err)
				require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
				require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
			}
			owned := metav1.IsControlledBy(worktree, workspace)
			// Generated Worktrees from run and mount carry the same UID-based GC
			// contract at creation; labels never drive deletion.
			assert.Equal(t, source == ownershipRunRepository || source == ownershipMountRepository, owned)
			if owned {
				assert.Contains(t, worktree.Finalizers, "repositories.rc.ayaka.io/worktree-delete-protection")
			}
			var output bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(ctx)
			cmd.SetErr(&output)
			require.NoError(t, deleteWorkspace(cmd, kube, workspace, false, func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) { return nil, nil }))
			remaining := new(repositoriesv1alpha1.Worktree)
			err := kube.Get(ctx, client.ObjectKeyFromObject(worktree), remaining)
			require.NoError(t, err, "the CLI delegates owned deletion to GC instead of issuing label-based DELETEs")
			if source == ownershipMountExisting || source == ownershipRunWorktree {
				assert.Empty(t, remaining.OwnerReferences)
			}
			if owned {
				assert.Contains(t, output.String(), "cascade delete worktree/"+worktree.Name)
			} else {
				assert.Contains(t, output.String(), "retain worktree/"+worktree.Name)
			}
		})
	}
}

func TestWorkspaceDeletionRetainsLegacyAndReusedNameResources(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: ownershipTestNamespace, UID: "new-uid"}}
	legacy := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: ownershipTestNamespace, Labels: map[string]string{worktreeownership.GeneratedForLabel: workspace.Name}}}
	oldOwner := workspace.DeepCopy()
	oldOwner.UID = "old-uid"
	reused := generatedWorkspaceWorktree(oldOwner, &repositoriesv1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: ownershipTestRepository, Namespace: ownershipTestNamespace}}, ownershipTestRepository, nil)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, legacy, reused).Build()
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetErr(&output)
	require.NoError(t, deleteWorkspace(cmd, kube, workspace, false, func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) { return nil, nil }))
	for _, resource := range []*repositoriesv1alpha1.Worktree{legacy, reused} {
		current := new(repositoriesv1alpha1.Worktree)
		require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(resource), current))
		assert.True(t, current.DeletionTimestamp.IsZero(), "a name or label never authorizes deletion")
		assert.Contains(t, output.String(), "retain worktree/"+resource.Name)
	}
	assert.NotContains(t, output.String(), "cascade delete worktree/")
}

func TestWorkspaceDeletionRejectsSharedOwnedWorktreeBeforeStopping(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: ownershipTestNamespace, UID: "dev-uid"}}
	worktree := generatedWorkspaceWorktree(workspace, &repositoriesv1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: ownershipTestRepository, Namespace: ownershipTestNamespace}}, ownershipTestRepository, nil)
	other := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: ownershipTestNamespace}, Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: "code", ReadOnly: true, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}}}}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, worktree, other).Build()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	stopped := false
	err := deleteWorkspace(cmd, kube, workspace, false, func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) {
		stopped = true
		return nil, nil
	})
	require.ErrorContains(t, err, "worktree detach")
	assert.False(t, stopped)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
	assert.True(t, workspace.DeletionTimestamp.IsZero())
}

func TestWorkspaceDeletionWarnsAboutDeprecatedCascadeFlag(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: ownershipTestNamespace}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetErr(&output)
	require.NoError(t, deleteWorkspace(cmd, kube, workspace, true, func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) { return nil, nil }))
	assert.Contains(t, output.String(), "--cascade-created-worktrees is deprecated")
}

func TestWorkspaceDeletionRefreshesResourceVersionAfterStopping(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: ownershipTestNamespace, UID: "workspace-uid"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(workspace).Build()
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetErr(new(bytes.Buffer))
	require.NoError(t, deleteWorkspace(cmd, kube, workspace, false, func(ctx context.Context, stale *workspacesv1alpha1.Workspace) ([]string, error) {
		current := new(workspacesv1alpha1.Workspace)
		require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(stale), current))
		current.Status.RuntimePodName = "stopped-pod"
		require.NoError(t, kube.Status().Update(ctx, current))
		return []string{"exec-1"}, nil
	}))
	current := new(workspacesv1alpha1.Workspace)
	err := kube.Get(t.Context(), client.ObjectKeyFromObject(workspace), current)
	assert.Error(t, err)
}
