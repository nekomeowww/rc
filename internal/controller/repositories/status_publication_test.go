package repositories

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/holdset"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

const (
	foreignFinalizer  = "example.com/hold"
	statusExecName    = "status-exec"
	statusParentName  = "status-parent"
	statusVolumeName  = "status-data"
	statusNoopCommand = "noop"
)

func statusTime(minute int) metav1.Time {
	return metav1.NewTime(time.Date(2026, time.March, 1, 12, minute, 0, 0, time.UTC))
}

func runningWorktreeExec(t *testing.T, scheme *runtime.Scheme) (*repositoriesv1alpha1.WorktreeExec, *batchv1.Job) {
	t.Helper()
	exec := &repositoriesv1alpha1.WorktreeExec{
		ObjectMeta: metav1.ObjectMeta{Name: statusExecName, Namespace: ownershipNamespace, UID: "exec-uid"},
		Spec:       repositoriesv1alpha1.WorktreeExecSpec{WorktreeRef: repositoriesv1alpha1.WorktreeReference{Name: ownershipWorktreeName}, Command: []string{statusNoopCommand}},
		Status: repositoriesv1alpha1.WorktreeExecStatus{JobName: statusExecName, Conditions: []metav1.Condition{{
			Type: repositoriesv1alpha1.WorktreeExecConditionSucceeded, Status: metav1.ConditionUnknown, Reason: "CommandRunning", LastTransitionTime: statusTime(0),
		}}},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: statusExecName, Namespace: ownershipNamespace}}
	require.NoError(t, controllerutil.SetControllerReference(exec, job, scheme))
	return exec, job
}

func TestWorktreeExecRecordsCompletedAtOnce(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		status batchv1.JobStatus
		want   metav1.Time
	}{
		{
			name: "succeeded Job uses its completion time",
			status: batchv1.JobStatus{CompletionTime: new(statusTime(5)), Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: statusTime(6)},
			}},
			want: statusTime(5),
		},
		{
			name: "failed Job uses its failure transition time",
			status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: statusTime(7)},
			}},
			want: statusTime(7),
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := t.Context()
			scheme := ownershipScheme(t)
			exec, job := runningWorktreeExec(t, scheme)
			job.Status = scenario.status
			kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(exec, job).WithObjects(exec, job).Build()
			r := &WorktreeExecReconciler{Client: kube, APIReader: kube, Scheme: scheme}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(exec)}

			_, err := r.Reconcile(ctx, request)
			require.NoError(t, err)
			current := new(repositoriesv1alpha1.WorktreeExec)
			require.NoError(t, kube.Get(ctx, request.NamespacedName, current))
			require.NotNil(t, current.Status.CompletedAt)
			assert.True(t, current.Status.CompletedAt.Equal(&scenario.want))
			condition := meta.FindStatusCondition(current.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded)
			require.NotNil(t, condition)
			assert.NotEqual(t, metav1.ConditionUnknown, condition.Status)

			// A later terminal write or reconcile never moves the completion time.
			require.NoError(t, worktreeExecStatus.setAt(ctx, kube, request.NamespacedName, metav1.ConditionFalse, "Other", "other", statusExecName, new(statusTime(30))))
			_, err = r.Reconcile(ctx, request)
			require.NoError(t, err)
			require.NoError(t, kube.Get(ctx, request.NamespacedName, current))
			assert.True(t, current.Status.CompletedAt.Equal(&scenario.want))
		})
	}
}

func TestWorktreeExecJobLostRecordsCompletedAt(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	exec, _ := runningWorktreeExec(t, scheme)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(exec).WithObjects(exec).Build()
	r := &WorktreeExecReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	before := metav1.NewTime(time.Now().Add(-time.Second))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(exec)})
	require.NoError(t, err)
	current := new(repositoriesv1alpha1.WorktreeExec)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(exec), current))
	assert.True(t, meta.IsStatusConditionFalse(current.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded))
	require.NotNil(t, current.Status.CompletedAt, "a failure without a finished Job records the time it was written")
	assert.False(t, current.Status.CompletedAt.Before(&before))
}

func TestWorktreeExecBackfillsCompletedAtFromSucceededCondition(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	exec, _ := runningWorktreeExec(t, scheme)
	exec.Status.Conditions[0] = metav1.Condition{Type: repositoriesv1alpha1.WorktreeExecConditionSucceeded, Status: metav1.ConditionTrue, Reason: "CommandSucceeded", LastTransitionTime: statusTime(9)}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(exec).WithObjects(exec).Build()
	r := &WorktreeExecReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(exec)}

	for range 2 {
		_, err := r.Reconcile(ctx, request)
		require.NoError(t, err)
		current := new(repositoriesv1alpha1.WorktreeExec)
		require.NoError(t, kube.Get(ctx, request.NamespacedName, current))
		require.NotNil(t, current.Status.CompletedAt)
		want := statusTime(9)
		assert.True(t, current.Status.CompletedAt.Equal(&want), "backfill keeps the original result time")
	}
}

func TestRepositoryExecCompletedAt(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	newExec := func(name string, conditions ...metav1.Condition) *repositoriesv1alpha1.RepositoryExec {
		return &repositoriesv1alpha1.RepositoryExec{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ownershipNamespace, UID: types.UID(name + "-uid"), Finalizers: []string{repositoryOperationFinalizer}},
			Spec:       repositoriesv1alpha1.RepositoryExecSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: statusParentName}, Command: []string{statusNoopCommand}},
			Status:     repositoriesv1alpha1.RepositoryExecStatus{JobName: name, Conditions: conditions},
		}
	}
	running := newExec("running", metav1.Condition{Type: repositoriesv1alpha1.RepositoryExecConditionSucceeded, Status: metav1.ConditionUnknown, Reason: "CommandRunning", LastTransitionTime: statusTime(0)})
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: running.Name, Namespace: ownershipNamespace}, Status: batchv1.JobStatus{
		CompletionTime: new(statusTime(3)),
		Conditions:     []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: statusTime(3)}},
	}}
	require.NoError(t, controllerutil.SetControllerReference(running, job, scheme))
	legacy := newExec("legacy", metav1.Condition{Type: repositoriesv1alpha1.RepositoryExecConditionSucceeded, Status: metav1.ConditionFalse, Reason: "CommandFailed", LastTransitionTime: statusTime(4)})
	legacy.Status.JobName = ""
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(running, legacy, job).WithObjects(running, legacy, job).Build()
	r := &RepositoryExecReconciler{Client: kube, APIReader: kube, Scheme: scheme}

	for _, scenario := range []struct {
		exec *repositoriesv1alpha1.RepositoryExec
		want metav1.Time
	}{{exec: running, want: statusTime(3)}, {exec: legacy, want: statusTime(4)}} {
		for range 2 {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(scenario.exec)})
			require.NoError(t, err)
			current := new(repositoriesv1alpha1.RepositoryExec)
			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(scenario.exec), current))
			require.NotNil(t, current.Status.CompletedAt, scenario.exec.Name)
			assert.True(t, current.Status.CompletedAt.Equal(&scenario.want), scenario.exec.Name)
		}
	}
}

func deletionBlocked(t *testing.T, kube client.Client, object client.Object) *metav1.Condition {
	t.Helper()
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(object), object))
	switch typed := object.(type) {
	case *repositoriesv1alpha1.Worktree:
		return meta.FindStatusCondition(typed.Status.Conditions, repositoriesv1alpha1.WorktreeConditionDeletionBlocked)
	case *repositoriesv1alpha1.Repository:
		return meta.FindStatusCondition(typed.Status.Conditions, repositoriesv1alpha1.RepositoryConditionDeletionBlocked)
	}
	t.Fatalf("unexpected object %T", object)
	return nil
}

func TestWorktreeDeletionBlockedNamesBlockers(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	repository, worktree, claim := ownershipStorage(t)
	worktree.Finalizers = append(worktree.Finalizers, foreignFinalizer)
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: ownershipWorkspaceName, Namespace: ownershipNamespace, UID: ownershipWorkspaceUID},
		Spec:       workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: ownershipWorktreeName, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: ownershipWorktreeName}}}},
	}
	// A legacy Lease from an older controller still blocks after the holders drain.
	legacyWriter := "legacy-writer-uid"
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: worktreeownership.LegacyWriteLeaseName(worktree), Namespace: ownershipNamespace, Labels: map[string]string{worktreeownership.LegacyHolderLabel: "legacy-exec"}},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &legacyWriter},
	}
	writer := holdset.Holder{Kind: worktreeownership.KindWorktreeExec, Name: "busy-exec", UID: "busy-exec-uid", Mode: worktreeownership.Write}
	require.NoError(t, holdset.Encode(worktree, worktreeownership.HoldersAnnotation, holdset.State{Holders: []holdset.Holder{writer}}))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "reader", Namespace: ownershipNamespace},
		Spec:       corev1.PodSpec{Volumes: []corev1.Volume{{Name: statusVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(worktree, claim, pod).
		WithObjects(repository, worktree, claim, workspace, lease, pod).Build()
	r := &WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)}

	// A live Worktree never publishes DeletionBlocked.
	_, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	assert.Nil(t, deletionBlocked(t, kube, worktree))
	require.NoError(t, r.setDeletionBlocked(ctx, worktree, &cleanupWait{reason: "Probe", message: "probe"}))
	assert.Nil(t, deletionBlocked(t, kube, worktree), "DeletionBlocked is written only while deleting")

	require.NoError(t, kube.Delete(ctx, worktree))
	steps := []struct {
		reason  string
		message string
		unblock func()
	}{
		{repositoriesv1alpha1.DeletionBlockedReasonWaitingForWriter, "WorktreeExec/busy-exec", func() {
			require.NoError(t, (worktreeownership.MountAccess{Client: kube, Reader: kube}).Release(ctx, request.NamespacedName, worktree.UID, writer.Key()))
		}},
		{repositoriesv1alpha1.DeletionBlockedReasonWaitingForMounts, ownershipWorkspaceName, func() { require.NoError(t, kube.Delete(ctx, workspace)) }},
		{repositoriesv1alpha1.DeletionBlockedReasonWaitingForWriter, "legacy-exec", func() { require.NoError(t, kube.Delete(ctx, lease)) }},
		{repositoriesv1alpha1.DeletionBlockedReasonWaitingForPods, pod.Name, func() { require.NoError(t, kube.Delete(ctx, pod)) }},
	}
	for _, step := range steps {
		result, err := r.Reconcile(ctx, request)
		require.NoError(t, err)
		assert.Positive(t, result.RequeueAfter)
		condition := deletionBlocked(t, kube, worktree)
		require.NotNil(t, condition, step.reason)
		assert.Equal(t, metav1.ConditionTrue, condition.Status)
		assert.Equal(t, step.reason, condition.Reason)
		assert.Contains(t, condition.Message, step.message)
		step.unblock()
	}

	// Cleanup completes: our finalizer is gone and no stale blocker remains,
	// even though a foreign finalizer keeps the object.
	for range 3 {
		_, err = r.Reconcile(ctx, request)
		require.NoError(t, err)
	}
	assert.Nil(t, deletionBlocked(t, kube, worktree))
	assert.NotContains(t, worktree.Finalizers, worktreeDeletionFinalizer)
}

func TestWorktreeDeletionBlockedWaitsForPendingVolume(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	repository, worktree, claim := ownershipStorage(t)
	claim.Status.Phase = corev1.ClaimPending
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree, claim).WithObjects(repository, worktree, claim).Build()
	r := &WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	require.NoError(t, kube.Delete(ctx, worktree))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
	require.NoError(t, err)
	condition := deletionBlocked(t, kube, worktree)
	require.NotNil(t, condition)
	assert.Equal(t, repositoriesv1alpha1.DeletionBlockedReasonWaitingForVolume, condition.Reason)
	assert.Contains(t, condition.Message, claim.Name)
}

func TestDirectPVCDeletionDoesNotPublishDeletionBlocked(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, claim := ownershipStorage(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reader", Namespace: ownershipNamespace}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: statusVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}}}}}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree, claim).WithObjects(worktree, claim, pod).Build()
	require.NoError(t, kube.Delete(ctx, claim))
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
	require.NoError(t, err)
	assert.Nil(t, deletionBlocked(t, kube, worktree), "storage deletion of a live Worktree is not Worktree deletion")
	assert.True(t, worktreeownership.MountsClosed(worktree))
}

func TestRepositoryDeletionBlockedReportsGarbageCollectionWaits(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: statusParentName, Namespace: ownershipNamespace, UID: "parent-uid", Finalizers: []string{metav1.FinalizerDeleteDependents}},
		Spec:       repositoriesv1alpha1.RepositorySpec{Storage: repositoriesv1alpha1.RepositoryStorageSpec{Size: resource.MustParse("1Gi"), StorageClassName: ownershipStorageClass}},
		Status:     repositoriesv1alpha1.RepositoryStatus{VolumeClaimName: statusParentName},
	}
	claim := parentVolumeClaim(repository, statusParentName)
	require.NoError(t, controllerutil.SetControllerReference(repository, claim, scheme))
	claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sync-pod", Namespace: ownershipNamespace},
		Spec:       corev1.PodSpec{Volumes: []corev1.Volume{{Name: workerVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: statusParentName}}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(repository, claim, pod).WithObjects(repository, claim, pod).Build()
	r := &RepositoryReconciler{Client: kube, APIReader: kube, Scheme: scheme, RunnerImage: ownershipRunnerImage}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(repository)}
	require.NoError(t, kube.Delete(ctx, repository))

	result, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter)
	condition := deletionBlocked(t, kube, repository)
	require.NotNil(t, condition)
	assert.Equal(t, repositoriesv1alpha1.DeletionBlockedReasonWaitingForPods, condition.Reason)
	assert.Contains(t, condition.Message, pod.Name)

	require.NoError(t, kube.Delete(ctx, pod))
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	condition = deletionBlocked(t, kube, repository)
	require.NotNil(t, condition)
	assert.Equal(t, repositoriesv1alpha1.DeletionBlockedReasonWaitingForVolume, condition.Reason)

	require.NoError(t, kube.Delete(ctx, claim))
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(claim), claim))
	claim.Finalizers = nil
	require.NoError(t, client.IgnoreNotFound(kube.Update(ctx, claim)))
	result, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.Nil(t, deletionBlocked(t, kube, repository))
}
