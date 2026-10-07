package repositories

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/volumeclaim"
)

func TestCreationRejectsPVCConflictBeforeCreatingCR(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"repository", "worktree"} {
		t.Run(kind, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, repositories.AddToScheme(scheme))
			source := &repositories.Repository{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: repositoryTestNamespace}}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: kind + "-blocked", Namespace: repositoryTestNamespace}}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, pvc).Build()
			var err error
			if kind == "repository" {
				_, err = (&RepositoryClient{Client: kube}).Clone(t.Context(), CloneRequest{Namespace: repositoryTestNamespace, Name: "blocked", URL: repositoryCloneTestURL})
			} else {
				_, err = (&WorktreeClient{Client: kube}).Start(t.Context(), WorktreeAddRequest{Namespace: repositoryTestNamespace, Name: "blocked", Repository: source.Name})
			}
			require.True(t, volumeclaim.IsConflict(err))
			require.ErrorContains(t, err, pvc.Name)
			parents := new(repositories.RepositoryList)
			require.NoError(t, kube.List(t.Context(), parents))
			require.Len(t, parents.Items, 1, "failed preflight must not leave an unhealthy Repository")
			children := new(repositories.WorktreeList)
			require.NoError(t, kube.List(t.Context(), children))
			require.Empty(t, children.Items, "failed preflight must not leave an unhealthy Worktree")
		})
	}
}
