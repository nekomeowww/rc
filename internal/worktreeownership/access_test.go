package worktreeownership

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func accessFixture(t *testing.T) (*runtime.Scheme, *repositoriesv1alpha1.Worktree, *workspacesv1alpha1.Workspace) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	return scheme, &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "test", UID: "child-uid", Finalizers: []string{"test/hold"}}}, &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "test", UID: "consumer-uid"}}
}

func TestMountAdmissionLosesCASRaceToDeletionFence(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "storage-fence", true: "kubernetes-delete"}[deleting], func(t *testing.T) {
			scheme, worktree, workspace := accessFixture(t)
			injected := false
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree, workspace).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if !injected {
					injected = true
					if deleting {
						require.NoError(t, c.Delete(ctx, worktree))
					} else {
						drained, err := (MountAccess{Client: c, Reader: c}).Close(ctx, worktree)
						require.NoError(t, err)
						require.True(t, drained)
					}
					// The API accepts desired state, but it grants no access to the volume.
					require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
					workspace.Spec.Mounts = []workspacesv1alpha1.WorkspaceMount{{Name: "late", WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}, ReadOnly: true}}
					require.NoError(t, c.Update(ctx, workspace))
				}
				return c.Patch(ctx, obj, patch, opts...)
			}}).Build()
			// ROOT CAUSE: a read-then-create protocol can accept an obsolete live object.
			// Both admission and fence must CAS the Worktree, including read-only mounts.
			admitted, err := (MountAccess{Client: kube, Reader: kube}).Admit(t.Context(), worktree, workspace)
			require.NoError(t, err)
			assert.True(t, injected)
			assert.False(t, admitted)
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(worktree), worktree))
			holders, err := mountHolders(worktree)
			require.NoError(t, err)
			assert.Empty(t, holders)
		})
	}
}

func TestAdmittedCreatorSurvivesFenceUntilExplicitRelease(t *testing.T) {
	scheme, worktree, workspace := accessFixture(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree, workspace).Build()
	gate := MountAccess{Client: kube, Reader: kube}
	admitted, err := gate.Admit(t.Context(), worktree, workspace)
	require.NoError(t, err)
	require.True(t, admitted)
	// No Pod is visible yet. Deletion must still wait for its admitted creator.
	drained, err := gate.Close(t.Context(), worktree)
	require.NoError(t, err)
	assert.False(t, drained)
	other := workspace.DeepCopy()
	other.UID = "replacement-uid"
	admitted, err = gate.Admit(t.Context(), worktree, other)
	require.NoError(t, err)
	assert.False(t, admitted)
	require.NoError(t, gate.ReleaseExcept(t.Context(), other, []repositoriesv1alpha1.Worktree{*worktree}, nil))
	drained, err = gate.Close(t.Context(), worktree)
	require.NoError(t, err)
	assert.False(t, drained, "same-name replacement cannot release the old incarnation")
	require.NoError(t, gate.ReleaseExcept(t.Context(), workspace, []repositoriesv1alpha1.Worktree{*worktree}, nil))
	drained, err = gate.Close(t.Context(), worktree)
	require.NoError(t, err)
	assert.True(t, drained)
	admitted, err = gate.Admit(t.Context(), worktree, workspace)
	require.NoError(t, err)
	assert.False(t, admitted, "releasing the last consumer never reopens a fence")
}

func TestReleaseOnlyTouchesExplicitCandidatesAndPreservesOtherMounts(t *testing.T) {
	scheme, removed, workspace := accessFixture(t)
	kept := removed.DeepCopy()
	kept.Name, kept.UID = "kept", "kept-uid"
	unrelated := removed.DeepCopy()
	unrelated.Name, unrelated.UID = "unrelated", "unrelated-uid"
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(removed, kept, unrelated, workspace).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			t.Fatal("release must use supplied candidates, not discover Worktrees")
			return nil
		},
	}).Build()
	gate := MountAccess{Client: kube, Reader: kube}
	for _, worktree := range []*repositoriesv1alpha1.Worktree{removed, kept, unrelated} {
		admitted, err := gate.Admit(t.Context(), worktree, workspace)
		require.NoError(t, err)
		require.True(t, admitted)
	}
	candidates := []repositoriesv1alpha1.Worktree{*removed, *kept}
	require.NoError(t, gate.ReleaseExcept(t.Context(), workspace, candidates, map[string]bool{kept.Name: true}))
	drained, err := gate.Close(t.Context(), removed)
	require.NoError(t, err)
	assert.True(t, drained)
	for _, worktree := range []*repositoriesv1alpha1.Worktree{kept, unrelated} {
		drained, err := gate.Close(t.Context(), worktree)
		require.NoError(t, err)
		assert.False(t, drained, "keep and non-candidates retain their reservations")
	}
	// A stale candidate must not release a same-name replacement's reservation.
	replaced := *unrelated.DeepCopy()
	replaced.UID = "previous-uid"
	require.NoError(t, gate.ReleaseExcept(t.Context(), workspace, []repositoriesv1alpha1.Worktree{replaced}, nil))
	drained, err = gate.Close(t.Context(), unrelated)
	require.NoError(t, err)
	assert.False(t, drained)
}
