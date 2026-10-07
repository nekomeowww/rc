package workspaces

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

func TestReadyWorkspaceAdmitsCurrentMountsWithoutListingWorktrees(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme, repositoriesv1alpha1.AddToScheme, workspacesv1alpha1.AddToScheme} {
		require.NoError(t, add(scheme))
	}
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "admission", Namespace: testNamespace, UID: "admission-uid", Generation: 1},
		Spec:       workspacesv1alpha1.WorkspaceSpec{Image: testRunnerImage, AutomountServiceAccountToken: boolPointer(false), Storage: &workspacesv1alpha1.PersistentStorageSpec{Size: resource.MustParse("1Gi")}},
	}
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: "mounted", Namespace: workspace.Namespace, UID: "mounted-uid"},
		Status:     repositoriesv1alpha1.WorktreeStatus{VolumeClaimName: "mounted", Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue}}},
	}
	workspace.Spec.Mounts = []workspacesv1alpha1.WorkspaceMount{{Name: worktree.Name, Path: worktree.Name, ReadOnly: true, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}}}
	home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	require.NoError(t, controllerutil.SetControllerReference(workspace, home, scheme))
	lists, patches, reads := 0, 0, 0
	failCheckpoint := false
	checkpointError := errors.New("injected Pod checkpoint failure")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace, &corev1.Pod{}).WithObjects(workspace, worktree, home).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*repositoriesv1alpha1.WorktreeList); ok {
				lists++
			}
			return c.List(ctx, list, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*repositoriesv1alpha1.Worktree); ok {
				reads++
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*corev1.Pod); ok && failCheckpoint {
				return checkpointError
			}
			if _, ok := obj.(*repositoriesv1alpha1.Worktree); ok {
				patches++
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	r := WorkspaceReconciler{Client: kube, APIReader: kube, Scheme: scheme, RunnerImage: testRunnerImage}
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	pod := new(corev1.Pod)
	require.NoError(t, kube.Get(ctx, req.NamespacedName, pod))
	pod.Spec.NodeName = "admission-node"
	require.NoError(t, kube.Update(ctx, pod))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, kube.Status().Update(ctx, pod))
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	helper := new(corev1.Pod)
	require.NoError(t, kube.Get(ctx, client.ObjectKey{Namespace: workspace.Namespace, Name: hotMountHelperName(workspace, hotWorktreeMount{name: worktree.Name})}, helper))
	helper.Status = pod.Status
	require.NoError(t, kube.Status().Update(ctx, helper))
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	// ROOT CAUSE: every Ready reconciliation scanned all Worktrees to release old
	// mounts. Stable topology only needs idempotent admission of current mounts.
	lists, patches, reads = 0, 0, 0
	for range 3 {
		_, err = r.Reconcile(ctx, req)
		require.NoError(t, err)
	}
	assert.Zero(t, lists, "stable Ready must not enumerate namespace Worktrees")
	assert.Zero(t, patches, "existing admissions are idempotent")
	assert.Positive(t, reads, "current mounts still pass admission checks")

	// A removed mount remains reserved until its helper has actually disappeared.
	require.NoError(t, kube.Get(ctx, req.NamespacedName, workspace))
	workspace.Spec.Mounts = nil
	workspace.Generation++
	require.NoError(t, kube.Update(ctx, workspace))
	helper.Finalizers = []string{"test/hold"}
	require.NoError(t, kube.Update(ctx, helper))
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	gate := worktreeownership.MountAccess{Client: kube}
	drained, err := gate.Close(ctx, worktree)
	require.NoError(t, err)
	assert.False(t, drained, "terminating helpers retain their admission")
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(helper), helper))
	helper.Finalizers = nil
	require.NoError(t, kube.Update(ctx, helper))
	// A failed checkpoint must keep the topology transition retryable.
	failCheckpoint = true
	_, err = r.Reconcile(ctx, req)
	require.ErrorIs(t, err, checkpointError)
	failCheckpoint = false
	r = WorkspaceReconciler{Client: kube, APIReader: kube, Scheme: scheme, RunnerImage: testRunnerImage}
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	drained, err = gate.Close(ctx, worktree)
	require.NoError(t, err)
	assert.True(t, drained, "topology cleanup releases a removed Worktree")
	lists = 0
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Zero(t, lists, "cleanup does not recur after topology converges")
}

func TestSuspendingWorkspaceReleasesClosedMount(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme, repositoriesv1alpha1.AddToScheme, workspacesv1alpha1.AddToScheme} {
		require.NoError(t, add(scheme))
	}
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: "closed", Namespace: testNamespace, UID: "closed-uid"}, Status: repositoriesv1alpha1.WorktreeStatus{VolumeClaimName: "closed", Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue}}}}
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "suspend-closed", Namespace: testNamespace, UID: "suspend-closed-uid"}, Spec: workspacesv1alpha1.WorkspaceSpec{Image: testRunnerImage, Storage: &workspacesv1alpha1.PersistentStorageSpec{Size: resource.MustParse("1Gi")}, DesiredState: workspacesv1alpha1.WorkspaceDesiredStateSuspended, Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: worktree.Name, Path: worktree.Name, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}, ReadOnly: true}}}}
	home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace}}
	require.NoError(t, controllerutil.SetControllerReference(workspace, home, scheme))
	require.NoError(t, controllerutil.SetControllerReference(workspace, pod, scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree, workspace, home).WithObjects(worktree, workspace, home, pod).Build()
	gate := worktreeownership.MountAccess{Client: kube}
	admitted, err := gate.Admit(ctx, worktree, workspace)
	require.NoError(t, err)
	require.True(t, admitted)
	drained, err := gate.Close(ctx, worktree)
	require.NoError(t, err)
	require.False(t, drained)
	r := WorkspaceReconciler{Client: kube, APIReader: kube, Scheme: scheme, RunnerImage: testRunnerImage}
	// ROOT CAUSE: dependency resolution rejected the deletion fence before the
	// suspend path could tear down its runtime and release the admitted mount.
	for range 3 {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
		require.NoError(t, err)
	}
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(pod), new(corev1.Pod))))
	drained, err = gate.Close(ctx, worktree)
	require.NoError(t, err)
	assert.True(t, drained, "suspension must drain storage even after admission closes")
}
