package repositories

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

const testGitLabRemote = "https://gitlab.com/acme/tools.git"

func TestResolveRepositoryAcceptsNameHostPathAndURL(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: "gitlab-com-acme-tools", Namespace: "default"},
		Spec:       repositoriesv1alpha1.RepositorySpec{Remote: repositoriesv1alpha1.RepositoryRemoteSpec{URL: testGitLabRemote}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository).Build()
	for _, selector := range []string{
		"gitlab-com-acme-tools",
		"gitlab.com/acme/tools",
		"acme/tools",
		"git@gitlab.com:acme/tools.git",
		"https://gitlab.com/acme/tools.git",
	} {
		resolved, err := ResolveRepository(context.Background(), kubeClient, "default", selector)
		require.NoError(t, err, selector)
		assert.Equal(t, repository.Name, resolved.Name, selector)
	}
}

func TestResolveRepositoryRejectsAmbiguousPath(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	first := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: "github-tools", Namespace: "default"},
		Spec:       repositoriesv1alpha1.RepositorySpec{Remote: repositoriesv1alpha1.RepositoryRemoteSpec{URL: "https://github.com/acme/tools.git"}},
	}
	second := first.DeepCopy()
	second.Name = "gitlab-tools"
	second.Spec.Remote.URL = testGitLabRemote
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(first, second).Build()

	_, err := ResolveRepository(context.Background(), kubeClient, "default", "acme/tools")
	assert.EqualError(t, err, `repository selector "acme/tools" is ambiguous; matches github-tools, gitlab-tools`)
}
