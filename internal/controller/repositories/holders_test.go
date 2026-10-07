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
	kube := indexedFake(scheme).WithStatusSubresource(worktree, claim).WithObjects(worktree, claim, workspace, lease).Build()
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
	kube := indexedFake(scheme).WithStatusSubresource(exec, worktree).WithObjects(worktree, exec, job, lease).Build()

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
	kube := indexedFake(scheme).WithObjects(worktree, workspace, lease, second).Build()
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
	kube := indexedFake(scheme).WithObjects(worktree, workspace, lease).Build()

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
		Spec:       corev1.PodSpec{Volumes: []corev1.Volume{{Name: "orphaned-data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	kube := indexedFake(scheme).WithStatusSubresource(pod).WithObjects(worktree, live, pod).Build()
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
	kube := indexedFake(scheme).WithObjects(repository, pod).Build()
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
	kube := indexedFake(scheme).WithStatusSubresource(worktree).WithObjects(worktree).Build()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)}
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	for range 2 {
		_, err := r.Reconcile(ctx, request)
		require.NoError(t, err)
	}
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, request.NamespacedName, new(repositoriesv1alpha1.Worktree))))
}

func TestWorktreeStatusMirrorsHolders(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	_, worktree, claim := ownershipStorage(t)
	workspace := writingWorkspace(worktree)
	kube := indexedFake(scheme).WithStatusSubresource(worktree).WithObjects(worktree, workspace).Build()
	r := WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	key := client.ObjectKeyFromObject(worktree)
	inUse := func() *metav1.Condition {
		current := new(repositoriesv1alpha1.Worktree)
		require.NoError(t, kube.Get(ctx, key, current))
		worktree = current
		return meta.FindStatusCondition(current.Status.Conditions, repositoriesv1alpha1.WorktreeConditionInUse)
	}

	require.NoError(t, r.publishUsage(ctx, key))
	condition := inUse()
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, repositoriesv1alpha1.WorktreeReasonIdle, condition.Reason)
	assert.Empty(t, worktree.Status.UsedBy)
	version := worktree.ResourceVersion
	require.NoError(t, r.publishUsage(ctx, key))
	inUse()
	assert.Equal(t, version, worktree.ResourceVersion, "status is written only on change")

	result, err := (worktreeownership.MountAccess{Client: kube, Reader: kube}).Admit(ctx, worktree, workspace, worktreeownership.WorkspaceHolder(workspace, worktreeownership.Write))
	require.NoError(t, err)
	require.True(t, result.Admitted)
	require.NoError(t, r.publishUsage(ctx, key))
	condition = inUse()
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, repositoriesv1alpha1.WorktreeReasonMountedByWorkspace, condition.Reason)
	require.Len(t, worktree.Status.UsedBy, 1)
	assert.Equal(t, repositoriesv1alpha1.UsageReference{Kind: worktreeownership.KindWorkspace, Name: workspace.Name, UID: workspace.UID, Mode: string(worktreeownership.Write), Since: worktree.Status.UsedBy[0].Since}, worktree.Status.UsedBy[0])
	assert.NotNil(t, worktree.Status.UsedBy[0].Since)

	require.NoError(t, (worktreeownership.MountAccess{Client: kube, Reader: kube}).ReleaseExcept(ctx, workspace, []repositoriesv1alpha1.Worktree{*worktree}, nil))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "outside-reader", Namespace: ownershipNamespace},
		Spec:       corev1.PodSpec{Volumes: []corev1.Volume{{Name: "outside-data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	require.NoError(t, kube.Create(ctx, pod))
	require.NoError(t, r.publishUsage(ctx, key))
	condition = inUse()
	assert.Equal(t, repositoriesv1alpha1.WorktreeReasonPodConsumer, condition.Reason)
	assert.Contains(t, condition.Message, pod.Name)
	assert.Empty(t, worktree.Status.UsedBy)
}

func TestRepositoryStatusMirrorsAccessWithoutInvalidatingAdmission(t *testing.T) {
	ctx := t.Context()
	scheme := ownershipScheme(t)
	repository, _, _ := ownershipStorage(t)
	repository.UID, repository.Generation = "mirrored-parent-uid", 1
	repository.Status.ObservedGeneration = 1
	repository.Status.Conditions = []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}}
	kube := indexedFake(scheme).WithStatusSubresource(repository).WithObjects(repository).Build()
	gate := repositoryaccess.Gate{Client: kube, Reader: kube}
	r := RepositoryReconciler{Client: kube, APIReader: kube, Scheme: scheme}
	key := client.ObjectKeyFromObject(repository)
	captured := repository.DeepCopy()

	first := holdset.Holder{Kind: repositoryaccess.KindWorktree, Name: "first-clone", UID: "first-clone-uid", Mode: repositoryaccess.Clone}
	admission, err := gate.Acquire(ctx, captured, first, true)
	require.NoError(t, err)
	require.Equal(t, repositoryaccess.Admitted, admission)
	require.NoError(t, r.publishAccess(ctx, key))
	current := new(repositoriesv1alpha1.Repository)
	require.NoError(t, kube.Get(ctx, key, current))
	require.NotNil(t, current.Status.Access)
	assert.Equal(t, string(repositoryaccess.Clone), current.Status.Access.Mode)
	require.Len(t, current.Status.Access.Holders, 1)
	assert.Equal(t, "first-clone", current.Status.Access.Holders[0].Name)

	// A consumer that captured the Repository before the mirror changed is
	// still admitted: status.access is an output, never an admission input.
	second := holdset.Holder{Kind: repositoryaccess.KindWorktree, Name: "second-clone", UID: "second-clone-uid", Mode: repositoryaccess.Clone}
	admission, err = gate.Acquire(ctx, captured, second, true)
	require.NoError(t, err)
	assert.Equal(t, repositoryaccess.Admitted, admission)

	require.NoError(t, gate.Release(ctx, current, first.Key()))
	require.NoError(t, gate.Release(ctx, current, second.Key()))
	require.NoError(t, r.publishAccess(ctx, key))
	require.NoError(t, kube.Get(ctx, key, current))
	assert.Nil(t, current.Status.Access)
}
