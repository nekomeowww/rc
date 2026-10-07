package repositories

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

func TestWorktreeClaimWatch(t *testing.T) {
	// ROOT CAUSE:
	// Every PVC event listed all Repositories and Worktrees in its namespace,
	// while child PVCs also passed through a separate Owns handler. Route children
	// directly by controller owner and query only indexed Repository dependents.
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: "watch-dependent", Namespace: metav1.NamespaceDefault},
		Spec:       repositoriesv1alpha1.WorktreeSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: "parent"}},
	}
	unrelated := worktree.DeepCopy()
	unrelated.Name = "unrelated"
	unrelated.Spec.RepositoryRef.Name = "another-repository"
	otherNamespace := worktree.DeepCopy()
	otherNamespace.Namespace = "other-namespace"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree, unrelated, otherNamespace).
		WithIndex(&repositoriesv1alpha1.Worktree{}, worktreeRepositoryIndex, worktreeRepositoryIndexValues).Build()
	controller := true
	repositoryOwner := metav1.OwnerReference{APIVersion: repositoriesv1alpha1.GroupVersion.String(), Kind: "Repository", Name: "parent", Controller: &controller}
	childOwner := repositoryOwner
	childOwner.Kind, childOwner.Name = "Worktree", worktree.Name
	foreignOwner := repositoryOwner
	foreignOwner.APIVersion = "unrelated.example/v1"
	nonController := repositoryOwner
	nonController.Controller = nil
	for _, tt := range []struct {
		name      string
		namespace string
		owner     *metav1.OwnerReference
		want      []reconcile.Request
		lists     int
	}{
		{name: "Repository-owned PVC", namespace: worktree.Namespace, owner: &repositoryOwner, want: []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(worktree)}}, lists: 1},
		{name: "source in other namespace", namespace: otherNamespace.Namespace, owner: &repositoryOwner, want: []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(otherNamespace)}}, lists: 1},
		{name: "Worktree-owned PVC", namespace: worktree.Namespace, owner: &childOwner, want: []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(worktree)}}},
		{name: "unowned", namespace: worktree.Namespace},
		{name: "foreign API group", namespace: worktree.Namespace, owner: &foreignOwner},
		{name: "non-controller owner", namespace: worktree.Namespace, owner: &nonController},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lists := 0
			checked := interceptor.NewClient(c, interceptor.Funcs{
				List: func(ctx context.Context, kubeClient client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists++
					require.IsType(t, &repositoriesv1alpha1.WorktreeList{}, list, "PVC events must never list Repositories")
					options := new(client.ListOptions).ApplyOptions(opts)
					require.Equal(t, tt.namespace, options.Namespace)
					require.NotNil(t, options.FieldSelector, "namespace-wide Worktree scans are forbidden")
					assert.Equal(t, "spec.repositoryRef.name=parent", options.FieldSelector.String())
					return kubeClient.List(ctx, list, opts...)
				},
			})
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "distinct-pvc-name", Namespace: tt.namespace}}
			if tt.owner != nil {
				claim.OwnerReferences = []metav1.OwnerReference{*tt.owner}
			}
			r := &WorktreeReconciler{Client: checked, APIReader: checked}
			assert.Equal(t, tt.want, r.worktreesForClaim(t.Context(), claim))
			assert.Equal(t, tt.lists, lists)
		})
	}
}
