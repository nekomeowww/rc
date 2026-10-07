//go:build integration

package workspaces

import (
	"context"
	"os"
	"testing"
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/rcplatform"
	workspaceservice "github.com/nekomeowww/rc/internal/workspaces"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestForegroundTranscriptCleanupWithGarbageCollector uses a dedicated Kind
// cluster: envtest has no GC. A collected dependent proves the real controller
// ran before asserting that the target-owned worker survives execution deletion.
func TestForegroundTranscriptCleanupWithGarbageCollector(t *testing.T) {
	path := os.Getenv("RC_TRANSCRIPT_GC_KUBECONFIG")
	if path == "" {
		t.Skip("requires the dedicated rc-t667-gc Kind cluster")
	}
	config, err := clientcmd.LoadFromFile(path)
	require.NoError(t, err)
	require.Equal(t, "kind-rc-t667-gc", config.CurrentContext, "never use an ambient cluster")
	restConfig, err := clientcmd.NewDefaultClientConfig(*config, &clientcmd.ConfigOverrides{}).ClientConfig()
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	kube, err := client.New(restConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := t.Context()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t667-gc-"}}
	require.NoError(t, kube.Create(ctx, namespace))
	t.Cleanup(func() { _ = kube.Delete(context.Background(), namespace) })
	target := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "gc-target", Namespace: namespace.Name, Finalizers: []string{"test.rc.ayaka.io/hold"}}, Spec: workspacesv1alpha1.WorkspaceSpec{ExecutionRetention: &workspacesv1alpha1.ExecutionRetentionPolicy{}}}
	require.NoError(t, kube.Create(ctx, target))
	t.Cleanup(func() {
		_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := kube.Get(context.Background(), client.ObjectKeyFromObject(target), target); err != nil {
				return client.IgnoreNotFound(err)
			}
			controllerutil.RemoveFinalizer(target, "test.rc.ayaka.io/hold")
			return kube.Update(context.Background(), target)
		})
	})

	// Prevent provisioning or scheduling: this test isolates API/GC semantics.
	storageClass := "deliberately-unbound"
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "gc-home", Namespace: namespace.Name}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &storageClass, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Mi")}}}}
	require.NoError(t, controllerutil.SetControllerReference(target, claim, scheme))
	require.NoError(t, kube.Create(ctx, claim))
	platform, err := rcplatform.Resolve(rcplatform.Target{OS: corev1.Linux})
	require.NoError(t, err)
	volume := workspaceservice.TranscriptVolume{Claim: claim.Name, Image: "unused-until-scheduled", Runtime: platform}
	service := &executionRetentionService{Client: kube, APIReader: kube}
	exec := &WorkspaceExecReconciler{Client: kube, APIReader: kube}
	newProcess := func(name string) *workspacesv1alpha1.WorkspaceExec {
		process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name, Finalizers: []string{executionFinalizer}}, Spec: workspacesv1alpha1.WorkspaceExecSpec{TargetRef: executionTargetReference(target), Command: []string{testTrueValue}}}
		require.NoError(t, controllerutil.SetControllerReference(target, process, scheme))
		require.NoError(t, kube.Create(ctx, process))
		process.Status.Phase = workspacesv1alpha1.WorkspaceExecPhaseSucceeded
		now := metav1.NewTime(time.Now().Add(-30 * 24 * time.Hour))
		process.Status.CompletedAt = &now
		process.Status.TranscriptVolumeClaimName, process.Status.TranscriptVolumeClaimUID = claim.Name, string(claim.UID)
		setTranscriptCondition(process, metav1.ConditionFalse, "Requested", "Waiting for batch")
		require.NoError(t, kube.Status().Update(ctx, process))
		t.Cleanup(func() {
			if kube.Get(context.Background(), client.ObjectKeyFromObject(process), process) == nil {
				process.Finalizers = nil
				_ = kube.Update(context.Background(), process)
			}
		})
		return process
	}
	process := newProcess("foreground-exec")
	batch := []processruntime.TranscriptIdentity{{ID: process.Name, UID: string(process.UID)}}
	require.NoError(t, service.createTranscriptWorker(ctx, target, volume, string(claim.UID), batch))
	key := client.ObjectKey{Namespace: namespace.Name, Name: transcriptCleanupName(target)}
	worker := new(corev1.Pod)
	require.NoError(t, kube.Get(ctx, key, worker))
	originalUID := worker.UID
	blocker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gc-blocker", Namespace: namespace.Name, Finalizers: []string{"test.rc.ayaka.io/hold"}}}
	require.NoError(t, controllerutil.SetControllerReference(process, blocker, scheme))
	require.NoError(t, kube.Create(ctx, blocker))
	t.Cleanup(func() {
		if kube.Get(context.Background(), client.ObjectKeyFromObject(blocker), blocker) == nil {
			blocker.Finalizers = nil
			_ = kube.Update(context.Background(), blocker)
		}
	})
	sentinel := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gc-sentinel", Namespace: namespace.Name}}
	require.NoError(t, controllerutil.SetControllerReference(process, sentinel, scheme))
	require.NoError(t, kube.Create(ctx, sentinel))
	require.NoError(t, kube.Delete(ctx, process, client.PropagationPolicy(metav1.DeletePropagationForeground)))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(sentinel), new(corev1.ConfigMap)))
	}, 30*time.Second, 50*time.Millisecond, "real foreground GC must collect this dependent")
	require.NoError(t, kube.Get(ctx, key, worker))
	require.True(t, worker.DeletionTimestamp.IsZero())
	require.Equal(t, originalUID, worker.UID)
	require.True(t, metav1.IsControlledBy(worker, target))
	service = &executionRetentionService{Client: kube, APIReader: kube}
	busy, err := service.reconcileTranscriptWorker(ctx, target)
	require.NoError(t, err)
	require.True(t, busy)
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := kube.Get(ctx, key, worker); err != nil {
			return err
		}
		worker.Status.Phase = corev1.PodSucceeded
		return kube.Status().Update(ctx, worker)
	}))
	_, err = service.reconcileTranscriptWorker(ctx, target)
	require.NoError(t, err)
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := exec.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)})
		return err
	}))
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(blocker), blocker))
	blocker.Finalizers = nil
	require.NoError(t, kube.Update(ctx, blocker))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(process), new(workspacesv1alpha1.WorkspaceExec)))
	}, 30*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { return apierrors.IsNotFound(kube.Get(ctx, key, new(corev1.Pod))) }, 30*time.Second, 50*time.Millisecond)
	// Whole-target foreground deletion must collect the worker and release the
	// execution even though that worker never completed its storage operation.
	process = newProcess("target-deleting-exec")
	require.NoError(t, service.createTranscriptWorker(ctx, target, volume, string(claim.UID), []processruntime.TranscriptIdentity{{ID: process.Name, UID: string(process.UID)}}))
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(target), target))
	require.NoError(t, kube.Delete(ctx, target, client.PropagationPolicy(metav1.DeletePropagationForeground)))
	require.Eventually(t, func() bool {
		err := kube.Get(ctx, key, worker)
		return apierrors.IsNotFound(err) || (err == nil && !worker.DeletionTimestamp.IsZero())
	}, 30*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		err := kube.Get(ctx, client.ObjectKeyFromObject(process), process)
		return err == nil && !process.DeletionTimestamp.IsZero()
	}, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := exec.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)})
		return err
	}))
	// GC mutates foreground finalizers concurrently with the test controller.
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(target), target); err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(target, "test.rc.ayaka.io/hold")
		return kube.Update(ctx, target)
	}))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(target), new(workspacesv1alpha1.Workspace)))
	}, 30*time.Second, 50*time.Millisecond, "no target/exec/worker foreground GC cycle")
}
