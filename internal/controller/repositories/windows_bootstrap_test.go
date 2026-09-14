package repositories

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

func TestWindowsDeferredBootstrapWaitsForRuntimeReadiness(t *testing.T) {
	t.Parallel()
	const workspaceName = "workspace"
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "test", Labels: map[string]string{"workspaces.rc.ayaka.io/generated-for": workspaceName}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: workspaceName, Namespace: "test", Labels: map[string]string{"workspaces.rc.ayaka.io/workspace": workspaceName}}, Spec: corev1.PodSpec{OS: &corev1.PodOS{Name: corev1.Windows}, Volumes: []corev1.Volume{{Name: "code", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "child"}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(worktree, pod).WithObjects(worktree, pod).Build()
	reconciler := &WorktreeReconciler{Client: kubeClient}
	ctx := context.Background()
	require.NoError(t, reconciler.reconcileWorkspaceBootstrap(ctx, worktree, "child", "source", "/repository"))
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	require.False(t, meta.IsStatusConditionTrue(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady))
	// Windows has no init-container termination records: readiness is published
	// only after lifecycle actions complete in the supervisor's writable layer.
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, kubeClient.Status().Update(ctx, pod))
	require.NoError(t, reconciler.reconcileWorkspaceBootstrap(ctx, worktree, "child", "source", "/repository"))
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	require.True(t, meta.IsStatusConditionTrue(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady))
}
