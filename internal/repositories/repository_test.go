package repositories

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

const (
	repositoryTestNamespace    = "default"
	repositoryTestName         = "gitlab-com-acme-tools"
	repositoryCloneTestURL     = "https://gitlab.com/acme/platform/tools.git"
	repositoryTestStorageClass = "truenas-nfs"
)

func TestRepositoryClientCloneCreatesRepository(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	repositoryClient := &RepositoryClient{Client: kubeClient}

	repository, err := repositoryClient.Clone(context.Background(), CloneRequest{
		Namespace:     repositoryTestNamespace,
		URL:           repositoryCloneTestURL,
		Ref:           "refs/heads/main",
		StorageClass:  repositoryTestStorageClass,
		Size:          resource.MustParse("10Gi"),
		CredentialRef: "gitlab-token",
	})

	require.NoError(t, err)
	assert.Equal(t, "gitlab-com-acme-platform-tools", repository.Name)

	persisted := new(repositoriesv1alpha1.Repository)
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKeyFromObject(repository), persisted))
	assert.Equal(t, repositoryCloneTestURL, persisted.Spec.Remote.URL)
	assert.Equal(t, "refs/heads/main", persisted.Spec.Ref)
	assert.Equal(t, repositoryTestStorageClass, persisted.Spec.Storage.StorageClassName)
	assert.Equal(t, resource.MustParse("10Gi"), persisted.Spec.Storage.Size)
	assert.Equal(t, &repositoriesv1alpha1.RepositoryCredentialReference{Name: "gitlab-token"}, persisted.Spec.Remote.CredentialRef)
}

func TestRepositoryClientCloneUsesCustomName(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	repository, err := (&RepositoryClient{Client: kubeClient}).Clone(context.Background(), CloneRequest{
		Namespace:    repositoryTestNamespace,
		URL:          repositoryCloneTestURL,
		Name:         "tools-main",
		StorageClass: repositoryTestStorageClass,
		Size:         resource.MustParse("10Gi"),
	})

	require.NoError(t, err)
	assert.Equal(t, "tools-main", repository.Name)
}

func TestRepositoryClientCloneMapsSubmoduleOptions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                string
		withSubmodules      bool
		recursiveSubmodules bool
		wantSubmodules      *repositoriesv1alpha1.RepositorySubmodulesSpec
	}{
		{name: "Disabled"},
		{name: "Direct", withSubmodules: true, wantSubmodules: &repositoriesv1alpha1.RepositorySubmodulesSpec{}},
		{name: "RecursiveImpliesEnabled", recursiveSubmodules: true, wantSubmodules: &repositoriesv1alpha1.RepositorySubmodulesSpec{Recursive: true}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			repositoryClient := &RepositoryClient{Client: kubeClient}

			repository, err := repositoryClient.Clone(context.Background(), CloneRequest{
				Namespace:           repositoryTestNamespace,
				URL:                 repositoryCloneTestURL,
				Name:                "tools-" + strings.ToLower(testCase.name),
				StorageClass:        repositoryTestStorageClass,
				Size:                resource.MustParse("10Gi"),
				WithSubmodules:      testCase.withSubmodules,
				RecursiveSubmodules: testCase.recursiveSubmodules,
			})

			require.NoError(t, err)
			assert.Equal(t, testCase.wantSubmodules, repository.Spec.Submodules)
		})
	}
}

func TestRepositoryClientWaitReturnsWhenRepositoryIsReady(t *testing.T) {
	t.Parallel()

	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: repositoryTestName, Namespace: repositoryTestNamespace},
		Status: repositoriesv1alpha1.RepositoryStatus{
			VolumeClaimName: repositoryTestName,
			Conditions: []metav1.Condition{{
				Type:   repositoriesv1alpha1.RepositoryConditionStorageReady,
				Status: metav1.ConditionTrue,
			}},
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository).Build()

	err := (&RepositoryClient{Client: kubeClient}).Wait(context.Background(), repository, io.Discard)

	require.NoError(t, err)
}

func TestRepositoryClientWaitReturnsBootstrapFailure(t *testing.T) {
	t.Parallel()

	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: repositoryTestName, Namespace: repositoryTestNamespace},
		Status: repositoriesv1alpha1.RepositoryStatus{
			Conditions: []metav1.Condition{{
				Type:    repositoriesv1alpha1.RepositoryConditionStorageReady,
				Status:  metav1.ConditionFalse,
				Reason:  bootstrapFailedReason,
				Message: "remote rejected the request",
			}},
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository).Build()

	err := (&RepositoryClient{Client: kubeClient}).Wait(context.Background(), repository, io.Discard)

	assert.EqualError(t, err, "repository failed: remote rejected the request")
}
