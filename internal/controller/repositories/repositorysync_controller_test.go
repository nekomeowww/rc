package repositories

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const syncTestRunnerImage = "sync-runner:test"

func syncFixture(t *testing.T) (client.Client, *repositoriesv1alpha1.Repository, *repositoriesv1alpha1.RepositorySync) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, coordinationv1.AddToScheme, corev1.AddToScheme, repositoriesv1alpha1.AddToScheme, configsv1alpha1.AddToScheme, workspacesv1alpha1.AddToScheme} {
		require.NoError(t, add(scheme))
	}
	repository := &repositoriesv1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "default", UID: "repository", Generation: 1}, Spec: repositoriesv1alpha1.RepositorySpec{
		Remote: repositoriesv1alpha1.RepositoryRemoteSpec{URL: "https://example.com/project.git"},
		Ref:    "refs/heads/main", Storage: repositoriesv1alpha1.RepositoryStorageSpec{StorageClassName: "csi", Size: resource.MustParse("1Gi")},
	}, Status: repositoriesv1alpha1.RepositoryStatus{ObservedGeneration: 1, VolumeClaimName: "source", Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	request := &repositoriesv1alpha1.RepositorySync{ObjectMeta: metav1.ObjectMeta{Name: "source-sync", Namespace: "default", UID: "sync", Generation: 1}, Spec: repositoriesv1alpha1.RepositorySyncSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repository.Name}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(repository, request, &batchv1.Job{}, &corev1.Pod{}, &repositoriesv1alpha1.RepositoryExec{}, &repositoriesv1alpha1.Worktree{}, &corev1.PersistentVolumeClaim{}).WithObjects(repository, request).Build()
	return c, repository, request
}

func reconcileSync(t *testing.T, c client.Client, request *repositoriesv1alpha1.RepositorySync) {
	t.Helper()
	r := RepositorySyncReconciler{Client: c, Scheme: c.Scheme(), RunnerImage: syncTestRunnerImage}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(request)})
	require.NoError(t, err)
}

func TestSyncRetainsResultAndBlocksExecAndClones(t *testing.T) {
	t.Parallel()
	c, repository, request := syncFixture(t)
	reconcileSync(t, c, request)
	job := new(batchv1.Job)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), job))
	require.Nil(t, job.Spec.TTLSecondsAfterFinished, "an unrecorded result must survive long controller outages")
	require.Contains(t, job.Spec.Template.Spec.Containers[0].Args, "refs/heads/main")
	gate := repositoryaccess.Gate{Client: c}
	admitted, err := gate.Acquire(t.Context(), repository, "new-clone", repositoryaccess.Clone, true)
	require.NoError(t, err)
	require.Equal(t, repositoryaccess.NotReserved, admitted)
	exec := &repositoriesv1alpha1.RepositoryExec{ObjectMeta: metav1.ObjectMeta{Name: "other-command", Namespace: repository.Namespace, UID: "other-command"}, Spec: repositoriesv1alpha1.RepositoryExecSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repository.Name}, Command: []string{"git", "status"}}}
	require.NoError(t, c.Create(t.Context(), exec))
	executor := RepositoryExecReconciler{Client: c, Scheme: c.Scheme(), RunnerImage: syncTestRunnerImage}
	_, err = executor.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(exec)})
	require.NoError(t, err)
	jobs := new(batchv1.JobList)
	require.NoError(t, c.List(t.Context(), jobs))
	require.Len(t, jobs.Items, 1)
	commit := strings.Repeat("a", 40)
	completed := metav1.Now()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sync-pod", Namespace: repository.Namespace, Labels: map[string]string{batchv1.JobNameLabel: job.Name}}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: job.Spec.Template.Spec.Containers[0].Name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: commit + "\n"}}}}}}
	require.NoError(t, controllerutil.SetControllerReference(job, pod, c.Scheme()))
	require.NoError(t, c.Create(t.Context(), pod))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	job.Status.CompletionTime = &completed
	require.NoError(t, c.Status().Update(t.Context(), job))
	reconcileSync(t, c, request)
	reconcileSync(t, c, request)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
	require.Equal(t, commit, request.Status.Commit)
	require.True(t, meta.IsStatusConditionTrue(request.Status.Conditions, repositoriesv1alpha1.RepositorySyncConditionSucceeded))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(repository), repository))
	require.True(t, meta.IsStatusConditionTrue(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady))
	require.NotNil(t, repository.Status.LastUpdatedAt)
	admitted, err = gate.Acquire(t.Context(), repository, "new-clone", repositoryaccess.Clone, true)
	require.NoError(t, err)
	require.Equal(t, repositoryaccess.Admitted, admitted)
	require.NoError(t, c.Delete(t.Context(), job))
	reconcileSync(t, c, request)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
	require.Equal(t, commit, request.Status.Commit)
	require.True(t, meta.IsStatusConditionTrue(request.Status.Conditions, repositoriesv1alpha1.RepositorySyncConditionSucceeded))
}

func TestSyncWaitsForAdmittedCloneAndRetryUsesNewRequest(t *testing.T) {
	t.Parallel()
	c, repository, request := syncFixture(t)
	gate := repositoryaccess.Gate{Client: c}
	acquired, err := gate.Acquire(t.Context(), repository, "pending-clone", repositoryaccess.Clone, true)
	require.NoError(t, err)
	require.Equal(t, repositoryaccess.Admitted, acquired)
	reconcileSync(t, c, request)
	jobs := new(batchv1.JobList)
	require.NoError(t, c.List(t.Context(), jobs))
	require.Empty(t, jobs.Items)
	require.NoError(t, gate.Release(t.Context(), repository.Namespace, "pending-clone"))
	reconcileSync(t, c, request)
	job := new(batchv1.Job)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), job))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "remote unavailable"}}
	require.NoError(t, c.Status().Update(t.Context(), job))
	reconcileSync(t, c, request)
	reconcileSync(t, c, request)
	retryRequest := &repositoriesv1alpha1.RepositorySync{ObjectMeta: metav1.ObjectMeta{Name: "retry-sync", Namespace: repository.Namespace, UID: "retry", Generation: 1}, Spec: request.Spec}
	require.NoError(t, c.Create(t.Context(), retryRequest))
	reconcileSync(t, c, retryRequest)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(retryRequest), new(batchv1.Job)))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
	require.True(t, meta.IsStatusConditionFalse(request.Status.Conditions, repositoriesv1alpha1.RepositorySyncConditionSucceeded))
}

func TestExistingWorktreeRemainsReadyDuringSync(t *testing.T) {
	t.Parallel()
	c, repository, request := syncFixture(t)
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: repository.Namespace, UID: "worktree", Generation: 1}, Spec: repositoriesv1alpha1.WorktreeSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repository.Name}}, Status: repositoriesv1alpha1.WorktreeStatus{ObservedGeneration: 1, VolumeClaimName: "existing", WorktreePath: "/repository", Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	require.NoError(t, c.Create(t.Context(), worktree))
	claim := worktreeVolumeClaim(worktree, repository.Name, "csi", resource.MustParse("1Gi"), []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany})
	require.NoError(t, controllerutil.SetControllerReference(worktree, claim, c.Scheme()))
	claim.Status.Phase = corev1.ClaimBound
	require.NoError(t, c.Create(t.Context(), claim))
	reconcileSync(t, c, request)
	controller := WorktreeReconciler{Client: c, Scheme: c.Scheme(), RunnerImage: syncTestRunnerImage}
	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
	require.NoError(t, err)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(worktree), worktree))
	require.True(t, meta.IsStatusConditionTrue(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady))
}

func TestSyncUsesOnlyConfiguredCredential(t *testing.T) {
	t.Parallel()
	c, repository, request := syncFixture(t)
	repository.Spec.Remote.CredentialRef = &repositoriesv1alpha1.RepositoryCredentialReference{Name: "private-source"}
	require.NoError(t, c.Update(t.Context(), repository))
	credential := &configsv1alpha1.Credential{ObjectMeta: metav1.ObjectMeta{Name: "private-source", Namespace: repository.Namespace}, Spec: configsv1alpha1.CredentialSpec{Type: configsv1alpha1.CredentialTypeSSHPrivateKey, SSHPrivateKey: &configsv1alpha1.SSHPrivateKeyCredential{PrivateKeyRef: configsv1alpha1.SecretKeyReference{Name: "source-key", Key: "private-key"}, KnownHostsRef: configsv1alpha1.SecretKeyReference{Name: "source-hosts", Key: "known-hosts"}}}}
	require.NoError(t, c.Create(t.Context(), credential))
	reconcileSync(t, c, request)
	job := new(batchv1.Job)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), job))
	require.Len(t, job.Spec.Template.Spec.Volumes, 3)
	require.Equal(t, "source-key", job.Spec.Template.Spec.Volumes[1].Secret.SecretName)
	require.Equal(t, "source-hosts", job.Spec.Template.Spec.Volumes[2].Secret.SecretName)
	require.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: repositoryGitSSHCommand, Value: "ssh -i /run/rc/credentials/ssh-private-key -o UserKnownHostsFile=/run/rc/credentials/ssh-known-hosts -o IdentitiesOnly=yes"})
}

// delayedJobCache reproduces the interval between Create and informer delivery.
type delayedJobCache struct{ client.Client }

func (c delayedJobCache) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*batchv1.Job); ok {
		return apierrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, key.Name)
	}
	return c.Client.Get(ctx, key, object, options...)
}

func TestSyncDoesNotTreatInformerDelayAsJobLoss(t *testing.T) {
	t.Parallel()
	c, _, request := syncFixture(t)
	reconcileSync(t, c, request)
	// A cached NotFound previously made the request fail while its real Job ran.
	r := RepositorySyncReconciler{Client: delayedJobCache{Client: c}, APIReader: c, Scheme: c.Scheme(), RunnerImage: syncTestRunnerImage}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(request)})
	require.NoError(t, err)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
	condition := meta.FindStatusCondition(request.Status.Conditions, repositoriesv1alpha1.RepositorySyncConditionSucceeded)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionUnknown, condition.Status)
}

func TestDeletingSyncWaitsForItsPods(t *testing.T) {
	t.Parallel()
	c, repository, request := syncFixture(t)
	reconcileSync(t, c, request)
	job := new(batchv1.Job)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), job))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "running-sync", Namespace: request.Namespace, Labels: map[string]string{batchv1.JobNameLabel: job.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	require.NoError(t, c.Create(t.Context(), pod))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
	require.NoError(t, c.Delete(t.Context(), request))
	reconcileSync(t, c, request)
	reconcileSync(t, c, request)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
	require.Contains(t, request.Finalizers, repositoryOperationFinalizer)
	gate := repositoryaccess.Gate{Client: c}
	busy, err := gate.Busy(t.Context(), repository, "another-writer")
	require.NoError(t, err)
	require.True(t, busy)
	pod.Status.Phase = corev1.PodFailed
	require.NoError(t, c.Status().Update(t.Context(), pod))
	reconcileSync(t, c, request)
	busy, err = gate.Busy(t.Context(), repository, "another-writer")
	require.NoError(t, err)
	require.False(t, busy)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(repository), repository))
	require.True(t, meta.IsStatusConditionFalse(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady))
	require.Equal(t, repositorySyncFailed, meta.FindStatusCondition(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady).Reason)
}

func TestDeletingSyncCleansUpAfterRepositoryDeletion(t *testing.T) {
	t.Parallel()
	c, repository, request := syncFixture(t)
	reconcileSync(t, c, request)
	require.NoError(t, c.Delete(t.Context(), repository))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(request), request))
	require.NoError(t, c.Delete(t.Context(), request))
	reconcileSync(t, c, request)
	reconcileSync(t, c, request)
	err := c.Get(t.Context(), client.ObjectKeyFromObject(request), new(repositoriesv1alpha1.RepositorySync))
	require.True(t, apierrors.IsNotFound(err), "a missing parent must not strand the request finalizer")
}
