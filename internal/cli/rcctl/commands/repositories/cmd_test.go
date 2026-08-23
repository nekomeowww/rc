package repositories

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

const (
	listTestNamespace = "development"
	deleteTestName    = "example"
)

func TestRepositoryListRowsReportStatusAndSortByName(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	updated := metav1.NewTime(time.Date(2026, time.August, 22, 1, 2, 3, 0, time.UTC))
	repositories := []repositoriesv1alpha1.Repository{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "zeta"},
			Spec: repositoriesv1alpha1.RepositorySpec{
				Remote: repositoriesv1alpha1.RepositoryRemoteSpec{URL: "ssh://git.example/zeta.git"}, Ref: "refs/heads/main",
			},
			Status: repositoriesv1alpha1.RepositoryStatus{
				VolumeClaimName: "zeta-parent", LastUpdatedAt: &updated,
				Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue}},
			},
		},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}},
	}

	rows := repositoryListRows(repositories)

	require.Len(t, rows, 2, "create one row per Repository")
	assertions.Equal("alpha", rows[0].name, "sort rows by Repository name")
	assertions.False(rows[0].ready, "report an absent Ready condition as false")
	assertions.Equal("-", rows[0].remote, "render an unavailable remote explicitly")
	assertions.Equal("zeta", rows[1].name, "retain the second sorted Repository")
	assertions.True(rows[1].ready, "report StorageReady=True")
	assertions.Equal("ssh://git.example/zeta.git", rows[1].remote, "report the configured remote")
	assertions.Equal("refs/heads/main", rows[1].ref, "report the configured ref")
	assertions.Equal("zeta-parent", rows[1].volume, "report the parent PVC")
	assertions.Equal("2026-08-22T01:02:03Z", rows[1].updated, "format the last sync timestamp")
}

func TestRepositoryListReturnsKubernetesAPIError(t *testing.T) {
	t.Parallel()
	kubeClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	err := runRepositoryList(context.Background(), io.Discard, kubeClient, listTestNamespace)

	require.Error(t, err, "listing an unregistered Repository type fails")
	assert.ErrorContains(t, err, "list Repositories", "identify the failed resource operation")
}

func TestRepositoryListCommandHasAliasAndRejectsArguments(t *testing.T) {
	t.Parallel()
	command := newListCommand(kubeconfig.NewFlags())

	assert.Contains(t, command.Aliases, "ls", "offer the conventional list alias")
	require.Error(t, command.Args(command, []string{"unexpected"}), "list accepts no positional arguments")
}

func TestRepositoryDeleteCommandHasAliasesAndRequiresName(t *testing.T) {
	t.Parallel()
	command := newDeleteCommand(kubeconfig.NewFlags())

	assert.Contains(t, command.Aliases, "remove", "offer remove as a descriptive alias")
	assert.Contains(t, command.Aliases, "rm", "offer rm as a conventional alias")
	require.Error(t, command.Args(command, nil), "delete requires a Repository name")
	require.NoError(t, command.Args(command, []string{deleteTestName}), "delete accepts exactly one Repository name")
	require.Error(t, command.Args(command, []string{"one", "two"}), "delete rejects extra positional arguments")
}

func TestRepositoryDeleteRemovesRepository(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: deleteTestName, Namespace: listTestNamespace},
	}).Build()

	err := runRepositoryDelete(context.Background(), kubeClient, listTestNamespace, deleteTestName)

	require.NoError(t, err, "delete the Repository")
	err = kubeClient.Get(context.Background(), client.ObjectKey{Namespace: listTestNamespace, Name: deleteTestName}, new(repositoriesv1alpha1.Repository))
	require.True(t, apierrors.IsNotFound(err), "the Repository no longer exists")
}

func TestRepositoryDeleteReturnsKubernetesAPIError(t *testing.T) {
	t.Parallel()
	kubeClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	err := runRepositoryDelete(context.Background(), kubeClient, listTestNamespace, deleteTestName)

	require.Error(t, err, "deleting an unregistered Repository type fails")
	assert.ErrorContains(t, err, "delete Repository", "identify the failed resource operation")
}
