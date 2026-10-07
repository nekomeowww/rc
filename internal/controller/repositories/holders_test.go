package repositories

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/holdset"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

// legacyWriteLease is the Lease an older controller or rcctl created for a
// Worktree writer, owned by that writer.
func legacyWriteLease(t *testing.T, worktree *repositoriesv1alpha1.Worktree, owner client.Object) *coordinationv1.Lease {
	t.Helper()
	holder := string(owner.GetUID())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: worktreeownership.LegacyWriteLeaseName(worktree), Namespace: worktree.Namespace, Labels: map[string]string{worktreeownership.LegacyHolderLabel: owner.GetName()}},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	require.NoError(t, controllerutil.SetControllerReference(owner, lease, ownershipScheme(t)))
	return lease
}

func writingWorkspace(worktree *repositoriesv1alpha1.Worktree) *workspacesv1alpha1.Workspace {
	return &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: ownershipWorkspaceName, Namespace: ownershipNamespace, UID: ownershipWorkspaceUID},
		Spec:       workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: worktree.Name, Path: worktree.Name, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}}}},
	}
}

func holdersOf(t *testing.T, reader client.Reader, worktree *repositoriesv1alpha1.Worktree) holdset.State {
	t.Helper()
	current := new(repositoriesv1alpha1.Worktree)
	require.NoError(t, reader.Get(t.Context(), client.ObjectKeyFromObject(worktree), current))
	state, err := worktreeownership.Decode(current)
	require.NoError(t, err)
	return state
}

func TestUpgradeImportsLegacyWriteLeaseAndMountAnnotationWithoutDeadlock(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, claim := ownershipStorage(t)
	workspace := writingWorkspace(worktree)
	// An older controller admitted the mount in mount-holders and the writer
	// in a Lease owned by the Workspace.
	worktree.Annotations = map[string]string{"repositories.rc.ayaka.io/mount-holders": `{"` + ownershipWorkspaceUID + `/` + ownershipWorkspaceName + `":true}`}
	lease := legacyWriteLease(t, worktree, workspace)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree, claim).WithObjects(worktree, claim, workspace, lease).Build()
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}

	changed, err := r.reconcileWorktreeHolders(ctx, worktree)
	require.NoError(t, err)
	assert.True(t, changed)
	state := holdersOf(t, kube, worktree)
	require.Len(t, state.Holders, 1, "the mount and the Lease are one Workspace holder")
	assert.Equal(t, worktreeownership.Write, state.Holders[0].Mode)
	assert.Equal(t, workspace.UID, state.Holders[0].UID)
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(lease), new(coordinationv1.Lease))), "the imported Lease is removed")
	current := new(repositoriesv1alpha1.Worktree)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), current))
	assert.NotContains(t, current.Annotations, "repositories.rc.ayaka.io/mount-holders")

	// Deletion waits for the imported writer, then finishes once the
	// Workspace unmounts and releases it: no deadlock on legacy state.
	require.NoError(t, kube.Delete(ctx, current))
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)}
	result, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter)
	require.NoError(t, kube.Get(ctx, request.NamespacedName, current))
	blocked := meta.FindStatusCondition(current.Status.Conditions, repositoriesv1alpha1.WorktreeConditionDeletionBlocked)
	require.NotNil(t, blocked)
	assert.Equal(t, repositoriesv1alpha1.DeletionBlockedReasonWaitingForWriter, blocked.Reason)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
	workspace.Spec.Mounts = nil
	require.NoError(t, kube.Update(ctx, workspace))
	require.NoError(t, (worktreeownership.MountAccess{Client: kube, Reader: kube}).ReleaseExcept(ctx, workspace, []repositoriesv1alpha1.Worktree{*current}, nil))
	for range 3 {
		_, err = r.Reconcile(ctx, request)
		require.NoError(t, err)
	}
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, request.NamespacedName, new(repositoriesv1alpha1.Worktree))))
}

func TestUpgradeImportsRunningExecLeaseAndKeepsItsJob(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, _ := ownershipStorage(t)
	worktree.Generation, worktree.Status.ObservedGeneration = 1, 1
	worktree.Status.Conditions[0].ObservedGeneration = 1
	exec := &repositoriesv1alpha1.WorktreeExec{
		ObjectMeta: metav1.ObjectMeta{Name: "upgraded-exec", Namespace: ownershipNamespace, UID: "upgraded-exec-uid"},
		Spec:       repositoriesv1alpha1.WorktreeExecSpec{WorktreeRef: repositoriesv1alpha1.WorktreeReference{Name: worktree.Name}, Command: []string{"probe-upgrade"}},
		Status: repositoriesv1alpha1.WorktreeExecStatus{JobName: "upgraded-exec", Conditions: []metav1.Condition{{
			Type: repositoriesv1alpha1.WorktreeExecConditionSucceeded, Status: metav1.ConditionUnknown, Reason: "JobCreatedBeforeUpgrade",
		}}},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: exec.Name, Namespace: exec.Namespace}}
	require.NoError(t, controllerutil.SetControllerReference(exec, job, scheme))
	lease := legacyWriteLease(t, worktree, exec)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(exec, worktree).WithObjects(worktree, exec, job, lease).Build()

	_, err := (&WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}).reconcileWorktreeHolders(ctx, worktree)
	require.NoError(t, err)
	state := holdersOf(t, kube, worktree)
	require.Len(t, state.Holders, 1)
	assert.Equal(t, worktreeownership.KindWorktreeExec, state.Holders[0].Kind)

	execs := WorktreeExecReconciler{Client: kube, APIReader: kube, Scheme: scheme, RunnerImage: ownershipRunnerImage}
	_, err = execs.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(exec)})
	require.NoError(t, err)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(job), job), "an imported writer keeps its running Job")
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(exec), exec))
	assert.Equal(t, "CommandRunning", meta.FindStatusCondition(exec.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded).Reason)
}

func TestLegacyLeaseFromOldRcctlStillBlocksSecondWriter(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, _ := ownershipStorage(t)
	worktree.Generation, worktree.Status.ObservedGeneration = 1, 1
	worktree.Status.Conditions[0].ObservedGeneration = 1
	// An old rcctl reserved the Worktree for a Workspace, then patched its spec.
	workspace := writingWorkspace(worktree)
	lease := legacyWriteLease(t, worktree, workspace)
	second := &repositoriesv1alpha1.WorktreeExec{ObjectMeta: metav1.ObjectMeta{Name: "second-writer", Namespace: ownershipNamespace, UID: "second-writer-uid"}, Spec: repositoriesv1alpha1.WorktreeExecSpec{WorktreeRef: repositoriesv1alpha1.WorktreeReference{Name: worktree.Name}, Command: []string{"probe-second"}}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree, workspace, lease, second).Build()
	execs := WorktreeExecReconciler{Client: kube, APIReader: kube, Scheme: scheme}

	admitted, err := execs.acquireClaim(ctx, second, worktree)
	require.NoError(t, err)
	assert.False(t, admitted, "a Lease written by an old rcctl blocks a second writer")
	_, err = (&WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}).reconcileWorktreeHolders(ctx, worktree)
	require.NoError(t, err)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	admitted, err = execs.acquireClaim(ctx, second, worktree)
	require.NoError(t, err)
	assert.False(t, admitted, "after import the reservation is a holder and still blocks")
	require.NoError(t, (worktreeownership.MountAccess{Client: kube, Reader: kube}).ReleaseExcept(ctx, workspace, []repositoriesv1alpha1.Worktree{*worktree}, nil))
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	admitted, err = execs.acquireClaim(ctx, second, worktree)
	require.NoError(t, err)
	assert.True(t, admitted)
}

func TestLegacyLeaseOfUnwantedOwnerKeepsBlockingUntilGone(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, _ := ownershipStorage(t)
	workspace := writingWorkspace(worktree)
	workspace.Spec.Mounts = nil
	lease := legacyWriteLease(t, worktree, workspace)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree, workspace, lease).Build()

	changed, err := (&WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}).reconcileWorktreeHolders(ctx, worktree)
	require.NoError(t, err)
	assert.False(t, changed, "an owner that no longer mounts the Worktree is not imported")
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(lease), lease), "the Lease keeps blocking until its owner or GC removes it")
	assert.Empty(t, holdersOf(t, kube, worktree).Holders)
}

func TestSweepNeverRemovesHolderWhosePodsRun(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, claim := ownershipStorage(t)
	live := writingWorkspace(worktree)
	gone := holdset.Holder{Kind: worktreeownership.KindWorktreeExec, Name: "force-deleted-exec", UID: "force-deleted-exec-uid", Mode: worktreeownership.Write}
	reader := worktreeownership.WorkspaceHolder(live, worktreeownership.Read)
	require.NoError(t, holdset.Encode(worktree, worktreeownership.HoldersAnnotation, holdset.State{Holders: []holdset.Holder{gone, reader}}))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "orphaned-writer", Namespace: ownershipNamespace},
		Spec:       corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(pod).WithObjects(worktree, live, pod).Build()
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}

	changed, err := r.reconcileWorktreeHolders(ctx, worktree)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Len(t, holdersOf(t, kube, worktree).Holders, 2, "an owner-less holder stays while a Pod still uses the volume")

	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, kube.Status().Update(ctx, pod))
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	changed, err = r.reconcileWorktreeHolders(ctx, worktree)
	require.NoError(t, err)
	assert.True(t, changed)
	state := holdersOf(t, kube, worktree)
	require.Len(t, state.Holders, 1, "a live owner is never swept")
	assert.Equal(t, reader.Key(), state.Holders[0].Key())
}

func TestRepositorySweepWaitsForParentConsumers(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	repository, _, _ := ownershipStorage(t)
	repository.UID = "swept-parent-uid"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "orphaned-sync", Namespace: ownershipNamespace},
		Spec:       corev1.PodSpec{Volumes: []corev1.Volume{{Name: "swept-parent", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: repository.Status.VolumeClaimName}}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository, pod).Build()
	gate := repositoryaccess.Gate{Client: kube, Reader: kube}
	gone := holdset.Holder{Kind: repositoryaccess.KindRepositorySync, Name: "force-deleted-sync", UID: "force-deleted-sync-uid", Mode: repositoryaccess.Write}
	_, err := holdset.Update(ctx, gate.Store(repository), func(_ client.Object, state *holdset.State) (bool, error) {
		state.Put(gone)
		return true, nil
	})
	require.NoError(t, err)
	r := RepositoryReconciler{Client: kube, APIReader: kube, Scheme: scheme}

	require.NoError(t, r.sweepRepositoryHolders(ctx, repository))
	busy, err := gate.Busy(ctx, repository, "next-writer")
	require.NoError(t, err)
	assert.True(t, busy, "a Pod may still be writing for the deleted owner")
	require.NoError(t, kube.Delete(ctx, pod))
	require.NoError(t, r.sweepRepositoryHolders(ctx, repository))
	busy, err = gate.Busy(ctx, repository, "next-writer")
	require.NoError(t, err)
	assert.False(t, busy, "a force-deleted owner no longer blocks writers")
}

func TestForceDeletedWorkspaceHolderDoesNotDeadlockDeletion(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, _ := ownershipStorage(t)
	now := metav1.Now()
	worktree.DeletionTimestamp = &now
	// The Workspace's finalizer was removed by hand before it released its holder.
	gone := worktreeownership.WorkspaceHolder(writingWorkspace(worktree), worktreeownership.Write)
	require.NoError(t, holdset.Encode(worktree, worktreeownership.HoldersAnnotation, holdset.State{Holders: []holdset.Holder{gone}}))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree).WithObjects(worktree).Build()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)}
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	for range 2 {
		_, err := r.Reconcile(ctx, request)
		require.NoError(t, err)
	}
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, request.NamespacedName, new(repositoriesv1alpha1.Worktree))))
}
