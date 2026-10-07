package worktreeownership

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func TestReferenceBlockersUseIndexAndSkipFinishedDeletions(t *testing.T) {
	scheme, worktree, _ := accessFixture(t)
	now := metav1.Now()
	mount := []workspacesv1alpha1.WorkspaceMount{{Name: "code", Path: "code", WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}}}
	workspaces := []client.Object{
		&workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: worktree.Namespace}, Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: mount}},
		&workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "tearing-down", Namespace: worktree.Namespace, DeletionTimestamp: &now, Finalizers: []string{WorkspaceCleanupFinalizer}}, Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: mount}},
		&workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "gc-only", Namespace: worktree.Namespace, DeletionTimestamp: &now, Finalizers: []string{metav1.FinalizerDeleteDependents}}, Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: mount}},
		&workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: worktree.Namespace}},
	}
	want := []string{"live", "tearing-down"}
	unindexed := 0
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspaces...).
		WithIndex(&workspacesv1alpha1.Workspace{}, WorkspaceWorktreeIndex, WorkspaceWorktreeIndexValues).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			options := new(client.ListOptions)
			options.ApplyOptions(opts)
			if options.FieldSelector == nil {
				unindexed++
			}
			return c.List(ctx, list, opts...)
		}}).Build()

	blockers, err := ReferenceBlockers(t.Context(), kube, worktree.Namespace, worktree.Name)
	require.NoError(t, err)
	assert.Equal(t, want, blockers, "a Workspace left only to garbage collection never blocks its Worktree")
	assert.Zero(t, unindexed, "the cache index replaces a namespace-wide Workspace scan")

	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspaces...).Build()
	blockers, err = ReferenceBlockers(t.Context(), direct, worktree.Namespace, worktree.Name)
	require.NoError(t, err)
	assert.Equal(t, want, blockers, "a reader without the index falls back to a scan")
}
