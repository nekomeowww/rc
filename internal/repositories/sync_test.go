package repositories

import (
	"context"
	"testing"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSyncWaitUsesRequestResultWithoutJobLogs(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&repositoriesv1alpha1.RepositorySync{}).Build()
	service := SyncClient{Client: c}
	request, err := service.Start(t.Context(), repositoryTestNamespace, "source")
	require.NoError(t, err)
	request.Status.JobName = "already-cleaned-job"
	request.Status.Commit = "resolved-commit"
	request.Status.Conditions = []metav1.Condition{{Type: repositoriesv1alpha1.RepositorySyncConditionSucceeded, Status: metav1.ConditionTrue, ObservedGeneration: request.Generation}}
	require.NoError(t, c.Status().Update(t.Context(), request))
	result, err := service.Wait(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, request.Status.Commit, result.Status.Commit)
}

func TestSyncWaitDoesNotAcceptPreviousRequest(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	old := &repositoriesv1alpha1.RepositorySync{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: repositoryTestNamespace}, Status: repositoriesv1alpha1.RepositorySyncStatus{Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.RepositorySyncConditionSucceeded, Status: metav1.ConditionTrue}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(old).Build()
	service := SyncClient{Client: c}
	request, err := service.Start(t.Context(), repositoryTestNamespace, "source")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = service.Wait(ctx, request)
	require.ErrorIs(t, err, context.Canceled)
}
