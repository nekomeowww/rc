package repositories

import (
	"testing"
	"time"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSyncExpiresSuccessfulAndFailedResults(t *testing.T) {
	t.Parallel()
	for _, status := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			c, repository, request := syncFixture(t)
			completed := metav1.NewTime(time.Now().Add(-4 * 24 * time.Hour))
			request.Status.CompletedAt = &completed
			request.Status.Conditions = []metav1.Condition{{Type: repositoriesv1alpha1.RepositorySyncConditionSucceeded, Status: status, LastTransitionTime: completed}}
			require.NoError(t, c.Status().Update(t.Context(), request))
			reconcileSync(t, c, request)
			require.True(t, apierrors.IsNotFound(c.Get(t.Context(), client.ObjectKeyFromObject(request), new(repositoriesv1alpha1.RepositorySync))))
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(repository), repository))
		})
	}
}

func TestSyncCustomRetentionSurvivesControllerRestart(t *testing.T) {
	t.Parallel()
	c, _, request := syncFixture(t)
	request.Spec.TTLSecondsAfterFinished = new(int32(7 * 24 * 60 * 60))
	require.NoError(t, c.Update(t.Context(), request))
	completed := metav1.NewTime(time.Now().Add(-4 * 24 * time.Hour))
	request.Status.CompletedAt = &completed
	request.Status.Conditions = []metav1.Condition{{Type: repositoriesv1alpha1.RepositorySyncConditionSucceeded, Status: metav1.ConditionTrue, LastTransitionTime: completed}}
	require.NoError(t, c.Status().Update(t.Context(), request))
	// A fresh reconciler must schedule the remaining time from persisted completion,
	// rather than delete at the default TTL or restart the full seven-day period.
	r := RepositorySyncReconciler{Client: c, Scheme: c.Scheme(), RunnerImage: syncTestRunnerImage}
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(request)})
	require.NoError(t, err)
	require.InDelta(t, (3 * 24 * time.Hour).Seconds(), result.RequeueAfter.Seconds(), 5)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
}

func TestSyncFailureWithoutJobGetsCompletionTime(t *testing.T) {
	t.Parallel()
	c, repository, request := syncFixture(t)
	require.NoError(t, c.Delete(t.Context(), repository))
	before := time.Now().Add(-time.Second)
	reconcileSync(t, c, request)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
	require.True(t, meta.IsStatusConditionFalse(request.Status.Conditions, repositoriesv1alpha1.RepositorySyncConditionSucceeded))
	require.Empty(t, request.Status.JobName)
	require.NotNil(t, request.Status.CompletedAt)
	require.True(t, request.Status.CompletedAt.After(before))
	r := RepositorySyncReconciler{Client: c, Scheme: c.Scheme(), RunnerImage: syncTestRunnerImage}
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(request)})
	require.NoError(t, err)
	require.InDelta(t, (3 * 24 * time.Hour).Seconds(), result.RequeueAfter.Seconds(), 5)
}

func TestSyncRetentionWaitsForRunningConsumer(t *testing.T) {
	t.Parallel()
	c, repository, request := syncFixture(t)
	request.Spec.TTLSecondsAfterFinished = new(int32(0))
	require.NoError(t, c.Update(t.Context(), request))
	reconcileSync(t, c, request)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
	require.True(t, request.DeletionTimestamp.IsZero(), "zero TTL does not delete an unfinished request")
	job := new(batchv1.Job)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), job))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	require.NoError(t, c.Status().Update(t.Context(), job))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "retention-consumer", Namespace: request.Namespace, Labels: map[string]string{batchv1.JobNameLabel: job.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	require.NoError(t, c.Create(t.Context(), pod))
	completed := metav1.NewTime(time.Now().Add(-time.Hour))
	request.Status.CompletedAt = &completed
	request.Status.Conditions = []metav1.Condition{{Type: repositoriesv1alpha1.RepositorySyncConditionSucceeded, Status: metav1.ConditionTrue, LastTransitionTime: completed}}
	require.NoError(t, c.Status().Update(t.Context(), request))
	reconcileSync(t, c, request)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
	require.Contains(t, request.Finalizers, repositoryOperationFinalizer)
	gate := repositoryaccess.Gate{Client: c}
	busy, err := gate.Busy(t.Context(), repository, "next-writer")
	require.NoError(t, err)
	require.True(t, busy)
	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, c.Status().Update(t.Context(), pod))
	reconcileSync(t, c, request)
	require.True(t, apierrors.IsNotFound(c.Get(t.Context(), client.ObjectKeyFromObject(request), new(repositoriesv1alpha1.RepositorySync))))
	busy, err = gate.Busy(t.Context(), repository, "next-writer")
	require.NoError(t, err)
	require.False(t, busy)
}

func TestSyncRetentionUsesRecordedTransitionForEarlierFailures(t *testing.T) {
	t.Parallel()
	c, _, request := syncFixture(t)
	request.Status.Conditions = []metav1.Condition{{Type: repositoriesv1alpha1.RepositorySyncConditionSucceeded, Status: metav1.ConditionFalse, LastTransitionTime: metav1.NewTime(time.Now().Add(-4 * 24 * time.Hour))}}
	require.NoError(t, c.Status().Update(t.Context(), request))
	reconcileSync(t, c, request)
	require.True(t, apierrors.IsNotFound(c.Get(t.Context(), client.ObjectKeyFromObject(request), new(repositoriesv1alpha1.RepositorySync))))
}
