//go:build ownershipgc

package repositories

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

// TestWorkspaceOwnershipWithRealGC requires an explicitly supplied kubeconfig
// for the task's isolated Kind cluster. Envtest has no garbage collector, so it
// cannot verify propagation ordering or descendant storage protection.
func TestWorkspaceOwnershipWithRealGC(t *testing.T) {
	kube := ownershipGCKube(t)
	scheme := kube.Scheme()
	for _, policy := range []metav1.DeletionPropagation{metav1.DeletePropagationBackground, metav1.DeletePropagationForeground} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := context.Background()
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ownership-"}}
			require.NoError(t, kube.Create(ctx, ns))
			t.Cleanup(func() {
				// testing.T cancels its context before cleanup callbacks run.
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				require.NoError(t, client.IgnoreNotFound(kube.Delete(cleanupCtx, ns)))
			})
			owner := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorkspaceName, Namespace: ns.Name}}
			require.NoError(t, kube.Create(ctx, owner))
			worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorktreeName, Namespace: ns.Name}, Spec: repositoriesv1alpha1.WorktreeSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: "repo"}, Branch: "feature"}}
			worktreeownership.InitializeGenerated(owner, worktree)
			require.NoError(t, kube.Create(ctx, worktree))
			consumer := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: ns.Name}, Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: ownershipWorktreeName, Path: ownershipWorktreeName, ReadOnly: true, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: ownershipWorktreeName}}}}}
			require.NoError(t, kube.Create(ctx, consumer))
			gate := worktreeownership.MountAccess{Client: kube, Reader: kube}
			admitted, err := gate.Admit(ctx, worktree, consumer)
			require.NoError(t, err)
			require.True(t, admitted, "reserve read-only access before any Pod exists")

			// A Pending claim is enough to expose foreground propagation. This uses no
			// storage driver and never provisions a volume or runs a workload.
			emptyClass := ""
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: worktree.Name, Namespace: ns.Name}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &emptyClass, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
			claim.Finalizers = []string{worktreeownership.VolumeProtectionFinalizer}
			require.NoError(t, controllerutil.SetControllerReference(worktree, claim, scheme))
			require.NoError(t, kube.Create(ctx, claim))
			require.NoError(t, kube.Delete(ctx, owner, client.PropagationPolicy(policy)))
			require.Eventually(t, func() bool {
				current := new(repositoriesv1alpha1.Worktree)
				return kube.Get(ctx, client.ObjectKeyFromObject(worktree), current) == nil && !current.DeletionTimestamp.IsZero()
			}, 20*time.Second, 100*time.Millisecond)
			r := &WorktreeReconciler{Client: kube, APIReader: kube, Scheme: scheme}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)}
			result, err := reconcileOwnershipGC(ctx, r, req)
			require.NoError(t, err)
			assert.Positive(t, result.RequeueAfter, "live consumer blocks finalization")
			// ROOT CAUSE: foreground GC may delete the PVC below a finalizing Worktree.
			// A Worktree finalizer alone cannot protect an owned descendant's data.
			assert.Never(t, func() bool {
				return apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(claim), new(corev1.PersistentVolumeClaim)))
			}, 2*time.Second, 100*time.Millisecond, "live consumer must retain its storage")
			late := consumer.DeepCopy()
			late.Name = "late-consumer"
			late.UID = ""
			late.ResourceVersion = ""
			require.NoError(t, kube.Create(ctx, late))
			admitted, err = gate.Admit(ctx, worktree, late)
			require.NoError(t, err)
			require.False(t, admitted, "a direct API Workspace created after GC starts cannot gain runtime access")
			require.NoError(t, kube.Delete(ctx, late))

			require.NoError(t, kube.Delete(ctx, consumer))
			require.NoError(t, gate.ReleaseExcept(ctx, consumer, []repositoriesv1alpha1.Worktree{*worktree}, nil))
			// A background cascade keeps a Pending clone admitted until it completes.
			// Explicitly cancel this driverless fixture to exercise cancellation cleanup.
			require.NoError(t, client.IgnoreNotFound(kube.Delete(ctx, claim)))
			require.Eventually(t, func() bool {
				_, err := reconcileOwnershipGC(ctx, r, req)
				require.NoError(t, err)
				return apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(worktree), new(repositoriesv1alpha1.Worktree)))
			}, 20*time.Second, 100*time.Millisecond, "GC finishes after the last live consumer releases the mount")
			require.Eventually(t, func() bool {
				return apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(owner), new(workspacesv1alpha1.Workspace)))
			}, 20*time.Second, 100*time.Millisecond)

		})
	}
}

func ownershipGCKube(t *testing.T) client.Client {
	t.Helper()
	filename := os.Getenv("RC_OWNERSHIP_KUBECONFIG")
	require.NotEmpty(t, filename, "set RC_OWNERSHIP_KUBECONFIG to the isolated Kind kubeconfig")
	raw, err := clientcmd.LoadFromFile(filename)
	require.NoError(t, err)
	require.Equal(t, "kind-rc-t661-ownership", raw.CurrentContext, "never run against an existing development cluster")
	config, err := clientcmd.RESTConfigFromKubeConfig(mustReadOwnershipConfig(t, filename))
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	kube, err := client.New(config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	return kube
}

func mustReadOwnershipConfig(t *testing.T, filename string) []byte {
	t.Helper()
	data, err := os.ReadFile(filename)
	require.NoError(t, err)
	return data
}

func TestLivePVCDeletionWithRealAPI(t *testing.T) {
	ctx := t.Context()
	kube := ownershipGCKube(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "live-pvc-delete-"}}
	require.NoError(t, kube.Create(ctx, ns))
	t.Cleanup(func() {
		// testing.T cancels its context before cleanup callbacks run.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, client.IgnoreNotFound(kube.Delete(cleanupCtx, ns)))
	})
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: ownershipWorktreeName, Namespace: ns.Name, Finalizers: []string{worktreeDeletionFinalizer}}, Spec: repositoriesv1alpha1.WorktreeSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: "absent-parent"}, Branch: "feature"}}
	require.NoError(t, kube.Create(ctx, worktree))
	worktree.Status.VolumeClaimName = worktree.Name
	require.NoError(t, kube.Status().Update(ctx, worktree))
	emptyClass := ""
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: worktree.Name, Namespace: ns.Name, Finalizers: []string{worktreeownership.VolumeProtectionFinalizer, "test/retain"}}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &emptyClass, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
	require.NoError(t, controllerutil.SetControllerReference(worktree, claim, kube.Scheme()))
	require.NoError(t, kube.Create(ctx, claim))
	require.NoError(t, kube.Delete(ctx, claim))
	r := &WorktreeReconciler{Client: kube, APIReader: kube, Scheme: kube.Scheme()}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)}
	_, err := reconcileOwnershipGC(ctx, r, req)
	require.NoError(t, err)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(claim), claim))
	assert.NotContains(t, claim.Finalizers, worktreeownership.VolumeProtectionFinalizer)
	assert.Contains(t, claim.Finalizers, "test/retain", "never remove another controller's finalizer")
	before := claim.DeepCopy()
	controllerutil.RemoveFinalizer(claim, "test/retain")
	require.NoError(t, kube.Patch(ctx, claim, client.MergeFrom(before)))
	require.Eventually(t, func() bool {
		_, err := reconcileOwnershipGC(ctx, r, req)
		require.NoError(t, err)
		return apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(claim), new(corev1.PersistentVolumeClaim)))
	}, 20*time.Second, 100*time.Millisecond)
	for range 3 {
		_, err := reconcileOwnershipGC(ctx, r, req)
		require.NoError(t, err)
	}
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(worktree), worktree))
	assert.True(t, worktree.DeletionTimestamp.IsZero())
	assert.True(t, worktreeownership.MountsClosed(worktree))
	assert.True(t, apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(claim), new(corev1.PersistentVolumeClaim))), "live Worktree must not recreate destroyed storage")
}

// reconcileOwnershipGC emulates the controller queue's retries for resourceVersion
// conflicts with Kubernetes GC/PVC protection. These tests drive reconcilers by
// hand; all other errors and non-convergence still fail the boundary assertions.
func reconcileOwnershipGC(ctx context.Context, r *WorktreeReconciler, req ctrl.Request) (ctrl.Result, error) {
	var result ctrl.Result
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error
		result, err = r.Reconcile(ctx, req)
		return err
	})
	return result, err
}
