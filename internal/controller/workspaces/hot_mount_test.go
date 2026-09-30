package workspaces

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
)

func TestHotWorktreeTopologyIgnoresReadyWorktreeMounts(t *testing.T) {
	t.Parallel()
	const firstMount = "code"
	const secondMount = "second"
	workspace := &workspacesv1alpha1.Workspace{
		Spec: workspacesv1alpha1.WorkspaceSpec{
			Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: firstMount, Path: firstMount}},
		},
	}
	resolved := &resolvedWorkspace{runtime: testRCPlatform(t), image: testRunnerImage}
	baseHash, err := workspaceTopologyHash(workspace, resolved)
	require.NoError(t, err)
	assert.NotEqual(t, "00a91221ed6ed3c1fa84ea61df8776d479c0b230f8c903495aed55a13bc0c386", baseHash, "controller upgrade changes the runtime topology")

	workspace.Spec.Mounts = append(workspace.Spec.Mounts, workspacesv1alpha1.WorkspaceMount{
		Name: secondMount, Path: secondMount, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: secondMount},
	})
	afterMountHash, err := workspaceTopologyHash(workspace, resolved)
	require.NoError(t, err)
	assert.Equal(t, baseHash, afterMountHash)

	resolved.staticWorktreeMounts = []workspacesv1alpha1.WorkspaceMount{workspace.Spec.Mounts[1]}
	deferredHash, err := workspaceTopologyHash(workspace, resolved)
	require.NoError(t, err)
	assert.NotEqual(t, baseHash, deferredHash)
}

func TestControllerUpgradeReplacesIdleWorkspaceRuntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, rbacv1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade", Namespace: testNamespace, UID: types.UID("upgrade-uid")},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			Image:   testRunnerImage,
			Storage: &workspacesv1alpha1.PersistentStorageSpec{StorageClassName: testStorageClass, Size: resource.MustParse("20Gi")},
		},
	}
	home := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace, OwnerReferences: []metav1.OwnerReference{{UID: workspace.UID, Controller: boolPointer(true)}}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace, Annotations: map[string]string{workspaceTopologyAnnotation: "platform-v2"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: workspacesv1alpha1.GroupVersion.String(), Kind: "Workspace", Name: workspace.Name, UID: workspace.UID, Controller: boolPointer(true)}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(workspace, home, oldPod).WithObjects(workspace, home, oldPod).Build()
	reconciler := &WorkspaceReconciler{Client: kubeClient, Scheme: scheme, RunnerImage: testRunnerImage}
	key := client.ObjectKeyFromObject(workspace)
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	require.Error(t, kubeClient.Get(ctx, key, new(corev1.Pod)), "replace the old runtime Pod")

	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	newPod := new(corev1.Pod)
	require.NoError(t, kubeClient.Get(ctx, key, newPod))
	assert.Equal(t, workspaceRuntimePolicyVersion, newPod.Annotations[workspaceRuntimePolicyAnnotation])
	assert.NotEmpty(t, newPod.Spec.Volumes[1].HostPath, "new Linux runtime supports hot Worktree mounts")
}

func TestHotWorktreeMountKeepsRuntimeAndActiveProcess(t *testing.T) {
	t.Parallel()
	const mountName = "new"
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, rbacv1.AddToScheme(scheme))
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))

	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "hot-workspace", Namespace: testNamespace, UID: types.UID("workspace-uid")},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			Image: testRunnerImage,
			Storage: &workspacesv1alpha1.PersistentStorageSpec{
				StorageClassName: testStorageClass, Size: resource.MustParse("20Gi"),
			},
		},
	}
	home := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace, OwnerReferences: []metav1.OwnerReference{{UID: workspace.UID, Controller: boolPointer(true)}}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(workspace, home, &corev1.Pod{}, &workspacesv1alpha1.WorkspaceExec{}, &repositoriesv1alpha1.Worktree{}).
		WithObjects(workspace, home).Build()
	reconciler := &WorkspaceReconciler{Client: kubeClient, Scheme: scheme, RunnerImage: testRunnerImage}
	key := client.ObjectKeyFromObject(workspace)
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	runtimePod := new(corev1.Pod)
	require.NoError(t, kubeClient.Get(ctx, key, runtimePod))
	assert.Empty(t, runtimePod.Spec.Volumes[0].HostPath, "home stays a PVC")
	runtimeUID := types.UID("runtime-uid")
	runtimePod.UID = runtimeUID
	runtimePod.Spec.NodeName = "node-a"
	require.NoError(t, kubeClient.Update(ctx, runtimePod))
	runtimePod.Status.Phase = corev1.PodRunning
	runtimePod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, kubeClient.Status().Update(ctx, runtimePod))

	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: workspace.Namespace},
		Spec: workspacesv1alpha1.WorkspaceExecSpec{
			TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name},
			Command:   []string{"sleep", "3600"},
		},
		Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseRunning},
	}
	require.NoError(t, kubeClient.Create(ctx, process))
	require.NoError(t, kubeClient.Status().Update(ctx, process))
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: "new-worktree", Namespace: workspace.Namespace, UID: types.UID("worktree-uid")},
		Status: repositoriesv1alpha1.WorktreeStatus{
			VolumeClaimName: "new-worktree", WorktreePath: repositoryRootMountPath,
			Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"}},
		},
	}
	require.NoError(t, kubeClient.Create(ctx, worktree))
	require.NoError(t, kubeClient.Status().Update(ctx, worktree))
	require.NoError(t, kubeClient.Get(ctx, key, workspace))
	workspace.Spec.Mounts = []workspacesv1alpha1.WorkspaceMount{{Name: mountName, Path: mountName, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}}}
	require.NoError(t, kubeClient.Update(ctx, workspace))

	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	require.NoError(t, kubeClient.Get(ctx, key, runtimePod))
	assert.Equal(t, runtimeUID, runtimePod.UID, "adding a Worktree retains the runtime Pod")
	mount := hotWorktreeMount{name: mountName, worktree: worktree.Name, path: mountName, claimName: worktree.Name}
	helper := new(corev1.Pod)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Name: hotMountHelperName(workspace, mount), Namespace: workspace.Namespace}, helper))
	assert.Equal(t, "node-a", helper.Spec.NodeName)
	assert.Equal(t, worktree.Name, helper.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	assert.True(t, *helper.Spec.Containers[0].SecurityContext.Privileged)
	assert.Equal(t, corev1.MountPropagationBidirectional, *helper.Spec.Containers[0].VolumeMounts[1].MountPropagation)
	lease := new(coordinationv1.Lease)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Name: worktreeclaim.LeaseName(worktree), Namespace: workspace.Namespace}, lease))
	assert.Equal(t, string(workspace.UID), *lease.Spec.HolderIdentity)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(process), process))
	assert.Equal(t, workspacesv1alpha1.WorkspaceExecPhaseRunning, process.Status.Phase)

	helper.Status.Phase = corev1.PodRunning
	helper.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, kubeClient.Status().Update(ctx, helper))
	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	require.NoError(t, kubeClient.Get(ctx, key, workspace))
	ready := meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, runtimeUID, runtimePod.UID)
}

func TestHotWorktreePathsCannotOverlap(t *testing.T) {
	t.Parallel()
	mounts := []workspacesv1alpha1.WorkspaceMount{
		{Name: "code", Path: "code", WorktreeRef: &workspacesv1alpha1.LocalReference{Name: "first"}},
		{Name: "nested", Path: "code/nested", WorktreeRef: &workspacesv1alpha1.LocalReference{Name: "second"}},
	}
	reason, _ := validateHotMountPaths(mounts)
	assert.Equal(t, "OverlappingMountPaths", reason)
}

func TestFailedHotMountCreatesCleanupPodWithoutPVC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "mount-workspace", Namespace: testNamespace, UID: types.UID("workspace-uid")},
	}
	helper := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-hot-test", Namespace: testNamespace},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{Env: []corev1.EnvVar{
				{Name: hotMountVisibleEnv, Value: "/publish-visible/code"},
				{Name: hotMountRootEnv, Value: "/publish-roots/worktree"},
			}}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).WithObjects(workspace).Build()
	reconciler := &WorkspaceReconciler{Client: kubeClient, Scheme: scheme, RunnerImage: testRunnerImage}
	cleaned, message, err := reconciler.cleanFailedHotMount(ctx, workspace, helper)
	require.NoError(t, err)
	assert.False(t, cleaned)
	assert.Equal(t, "Cleaning up failed hot Worktree mount", message)
	cleaner := new(corev1.Pod)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Name: helper.Name + "-clean", Namespace: testNamespace}, cleaner))
	assert.Equal(t, "node-a", cleaner.Spec.NodeName)
	assert.Len(t, cleaner.Spec.Volumes, 2, "cleanup does not require the failed Worktree PVC")
	for _, volume := range cleaner.Spec.Volumes {
		assert.NotNil(t, volume.HostPath)
	}
	cleaner.Status.Phase = corev1.PodSucceeded
	require.NoError(t, kubeClient.Status().Update(ctx, cleaner))
	cleaned, message, err = reconciler.cleanFailedHotMount(ctx, workspace, helper)
	require.NoError(t, err)
	assert.True(t, cleaned)
	assert.Empty(t, message)
}
