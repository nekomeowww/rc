package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/rckube"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	testTranscriptHomeClaim     = "original-home"
	testTranscriptHoldFinalizer = "test/hold"
)

type filesystemProcessRuntime struct {
	recordingProcessRuntime
	supervisor *rckube.Supervisor
	failure    error
}

func (r *filesystemProcessRuntime) PruneTranscript(_ context.Context, _ processruntime.Target, id, uid string) error {
	if r.failure != nil {
		return r.failure
	}
	return r.supervisor.PruneTranscript(id, uid)
}

// transcriptFixture keeps CRs longer than transcripts and binds the original
// home, so tests exercise storage independently of CR selection.
func transcriptFixture(t *testing.T) (client.WithWatch, *workspacesv1alpha1.Workspace, *workspacesv1alpha1.WorkspaceExec) {
	t.Helper()
	kube, ws, process := retentionFixture(t)
	ws.Spec.ExecutionRetention.TTLAfterFinished = &metav1.Duration{Duration: 60 * 24 * time.Hour}
	ws.Status.RuntimeImage, ws.Status.HomeVolumeClaimName = "runner", testTranscriptHomeClaim
	require.NoError(t, kube.Update(t.Context(), ws))
	require.NoError(t, kube.Status().Update(t.Context(), ws))
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testTranscriptHomeClaim, Namespace: testNamespace, UID: "claim-uid"}}
	require.NoError(t, kube.Create(t.Context(), claim))
	process.Finalizers = []string{executionFinalizer}
	require.NoError(t, controllerutil.SetControllerReference(ws, process, kube.Scheme()))
	require.NoError(t, kube.Update(t.Context(), process))
	process.Status.TranscriptVolumeClaimName, process.Status.TranscriptVolumeClaimUID = claim.Name, string(claim.UID)
	require.NoError(t, kube.Status().Update(t.Context(), process))
	return kube, ws, process
}

// ROOT CAUSE: per-execution workers scale with history and are killed by exec
// foreground GC. One stable target owns the bounded, restart-safe batch instead.
func TestTargetTranscriptBatchRetriesAndAcknowledgesBeforeRemoval(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	kube, ws, first := transcriptFixture(t)
	for i := 1; i < 23; i++ {
		p := first.DeepCopy()
		p.Name = fmt.Sprintf("entry-%02d", i)
		p.UID = types.UID(p.Name)
		p.ResourceVersion = ""
		require.NoError(t, kube.Create(ctx, p))
	}
	pinned := first.DeepCopy()
	pinned.Name, pinned.UID, pinned.ResourceVersion = "pinned", "pinned-uid", ""
	pinned.Spec.Retain = true
	require.NoError(t, kube.Create(ctx, pinned))
	r := &executionRetentionService{Client: kube}
	require.NoError(t, r.reconcileTranscripts(ctx, ws, ws.Spec.ExecutionRetention))
	key := client.ObjectKey{Namespace: ws.Namespace, Name: transcriptCleanupName(ws)}
	pod := new(corev1.Pod)
	require.NoError(t, kube.Get(ctx, key, pod))
	require.True(t, metav1.IsControlledBy(pod, ws))
	require.False(t, *pod.Spec.AutomountServiceAccountToken)
	require.False(t, pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly)
	var batch []processruntime.TranscriptIdentity
	require.NoError(t, json.Unmarshal([]byte(pod.Spec.Containers[0].Args[0]), &batch))
	require.Len(t, batch, executionCleanupBatch)
	pod.Status.Phase = corev1.PodFailed
	require.NoError(t, kube.Status().Update(ctx, pod))
	require.ErrorContains(t, r.reconcileTranscripts(ctx, ws, ws.Spec.ExecutionRetention), "cleanup Pod failed")
	r = &executionRetentionService{Client: kube}
	require.NoError(t, r.reconcileTranscripts(ctx, ws, ws.Spec.ExecutionRetention))
	require.NoError(t, kube.Get(ctx, key, pod))
	// Execute the actual offline batch on files; Pod phase here models the exit.
	directory := t.TempDir()
	for _, entry := range batch {
		require.NoError(t, os.MkdirAll(filepath.Join(directory, entry.ID), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(directory, entry.ID, "owner.uid"), []byte(entry.UID), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(directory, entry.ID, "transcript.log"), []byte("output"), 0o600))
	}
	require.NoError(t, rckube.RemoveTranscripts(directory, batch))
	for _, entry := range batch {
		require.NoFileExists(t, filepath.Join(directory, entry.ID, "transcript.log"))
		require.FileExists(t, filepath.Join(directory, entry.ID, "owner.uid"))
	}
	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, kube.Status().Update(ctx, pod))
	broken := interceptor.NewClient(kube, interceptor.Funcs{SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
		return errors.New("lost acknowledgement")
	}})
	require.ErrorContains(t, (&executionRetentionService{Client: broken}).reconcileTranscripts(ctx, ws, ws.Spec.ExecutionRetention), "lost acknowledgement")
	require.NoError(t, kube.Get(ctx, key, pod), "success survives a failed acknowledgement")
	require.NoError(t, (&executionRetentionService{Client: kube}).reconcileTranscripts(ctx, ws, ws.Spec.ExecutionRetention))
	require.True(t, apierrors.IsNotFound(kube.Get(ctx, key, pod)))
	require.NoError(t, r.reconcileTranscripts(ctx, ws, ws.Spec.ExecutionRetention))
	require.NoError(t, kube.Get(ctx, key, pod))
	require.NoError(t, json.Unmarshal([]byte(pod.Spec.Containers[0].Args[0]), &batch))
	require.Len(t, batch, 3, "remaining entries share the next worker")
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(pinned), pinned))
	require.False(t, transcriptRequested(pinned))
	require.False(t, transcriptCleaned(pinned))
	pods := new(corev1.PodList)
	require.NoError(t, kube.List(ctx, pods))
	require.Len(t, pods.Items, 1)
}

func TestDeletedExecUsesTargetServiceWithPolicyDisabled(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	kube, ws, process := transcriptFixture(t)
	ws.Spec.ExecutionRetention = nil
	require.NoError(t, kube.Update(ctx, ws))
	process.Spec.Retain = true
	require.NoError(t, kube.Update(ctx, process))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "live-runtime", Namespace: testNamespace, UID: "pod-uid", Labels: map[string]string{workspaceManagedByLabel: ws.Name}}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: transcriptHomeVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: testTranscriptHomeClaim}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	require.NoError(t, controllerutil.SetControllerReference(ws, pod, kube.Scheme()))
	require.NoError(t, kube.Create(ctx, pod))
	directory := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(directory, process.Name), 0o700))
	transcript := filepath.Join(directory, process.Name, "transcript.log")
	require.NoError(t, os.WriteFile(transcript, []byte("history"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, process.Name, "owner.uid"), []byte(process.UID), 0o600))
	require.NoError(t, kube.Delete(ctx, process))
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)}
	exec := &WorkspaceExecReconciler{Client: kube}
	_, err := exec.Reconcile(ctx, request)
	require.NoError(t, err)
	boundary := &filesystemProcessRuntime{supervisor: rckube.NewSupervisor(directory, time.Second), failure: errors.New("disk failure")}
	r := &executionRetentionService{Client: kube, Runtime: boundary}
	require.ErrorContains(t, r.reconcileTranscripts(ctx, ws, nil), "disk failure")
	require.FileExists(t, transcript)
	require.NoError(t, kube.Get(ctx, request.NamespacedName, process))
	require.True(t, transcriptRequested(process))
	r = &executionRetentionService{Client: kube, Runtime: &filesystemProcessRuntime{supervisor: rckube.NewSupervisor(directory, time.Second)}}
	require.NoError(t, r.reconcileTranscripts(ctx, ws, nil))
	_, err = exec.Reconcile(ctx, request)
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(kube.Get(ctx, request.NamespacedName, process)))
	require.NoFileExists(t, transcript)
}

func TestWholeStorageLifecycleReleasesExecutionWithoutWorker(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"target-deleting", "target-replaced", "pvc-deleting", "pvc-replaced", "pvc-missing"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			kube, ws, process := transcriptFixture(t)
			claim := new(corev1.PersistentVolumeClaim)
			require.NoError(t, kube.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testTranscriptHomeClaim}, claim))
			switch change {
			case "target-deleting":
				ws.Finalizers = []string{testTranscriptHoldFinalizer}
				require.NoError(t, kube.Update(ctx, ws))
				require.NoError(t, kube.Delete(ctx, ws))
			case "target-replaced":
				require.NoError(t, kube.Delete(ctx, ws))
				ws.UID = "new-target"
				ws.ResourceVersion = ""
				require.NoError(t, kube.Create(ctx, ws))
			case "pvc-deleting":
				claim.Finalizers = []string{testTranscriptHoldFinalizer}
				require.NoError(t, kube.Update(ctx, claim))
				require.NoError(t, kube.Delete(ctx, claim))
			case "pvc-replaced":
				require.NoError(t, kube.Delete(ctx, claim))
				claim.UID = "new-pvc"
				claim.ResourceVersion = ""
				require.NoError(t, kube.Create(ctx, claim))
			case "pvc-missing":
				require.NoError(t, kube.Delete(ctx, claim))
			}
			require.NoError(t, kube.Delete(ctx, process))
			_, err := (&WorkspaceExecReconciler{Client: kube}).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)})
			require.NoError(t, err)
			require.True(t, apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(process), process)))
			pods := new(corev1.PodList)
			require.NoError(t, kube.List(ctx, pods))
			require.Empty(t, pods.Items)
		})
	}
}

// A single Environment smoke covers typed routing, original draft identity,
// Windows placement and worker ownership; policy races use the shared service.
func TestEnvironmentRetentionUsesTypedTargetAndOriginalPVC(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	kube, ws, process := transcriptFixture(t)
	env := &workspacesv1alpha1.WorkspaceEnvironment{ObjectMeta: metav1.ObjectMeta{Name: ws.Name, Namespace: ws.Namespace, UID: "env-uid"}, Spec: workspacesv1alpha1.WorkspaceEnvironmentSpec{Image: "runner", OS: corev1.Windows, ExecutionRetention: ws.Spec.ExecutionRetention}, Status: workspacesv1alpha1.WorkspaceEnvironmentStatus{DraftVolumeClaimName: "new-draft"}}
	require.NoError(t, kube.Create(ctx, env))
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-editor", Namespace: env.Namespace, Labels: map[string]string{environmentManagedByLabel: env.Name}}, Spec: corev1.PodSpec{NodeName: "windows-node", Volumes: []corev1.Volume{{Name: transcriptHomeVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: testTranscriptHomeClaim}}}}}, Status: corev1.PodStatus{Phase: corev1.PodFailed}}
	require.NoError(t, controllerutil.SetControllerReference(env, oldPod, kube.Scheme()))
	require.NoError(t, kube.Create(ctx, oldPod))
	process.Spec.TargetRef = executionTargetReference(env)
	process.OwnerReferences = nil
	require.NoError(t, controllerutil.SetControllerReference(env, process, kube.Scheme()))
	require.NoError(t, kube.Update(ctx, process))
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(env)}
	require.Empty(t, retentionTargetForProcess(workspacesv1alpha1.WorkspaceExecTargetWorkspace)(ctx, process))
	require.Equal(t, []reconcile.Request{request}, retentionTargetForProcess(workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment)(ctx, process))
	_, err := (&WorkspaceEnvironmentRetentionReconciler{Client: kube}).Reconcile(ctx, request)
	require.NoError(t, err)
	pod := new(corev1.Pod)
	require.NoError(t, kube.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: transcriptCleanupName(env)}, pod))
	require.True(t, metav1.IsControlledBy(pod, env))
	require.Equal(t, corev1.Windows, pod.Spec.OS.Name)
	require.Equal(t, "windows-node", pod.Spec.NodeName, "reuse the stopped runtime RWO attachment")
	require.Equal(t, testTranscriptHomeClaim, pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
}

// ROOT CAUSE: persisting Requested is a separate write, not permission to
// dispatch a stale pin/policy decision. A newly pinned record must stay intact.
func TestTranscriptRechecksPinBeforeBatchDispatch(t *testing.T) {
	t.Parallel()
	for _, boundaryName := range []string{"a", "b"} {
		t.Run(boundaryName, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			kube, ws, process := transcriptFixture(t)
			if boundaryName == "b" {
				sibling := process.DeepCopy()
				sibling.Name, sibling.UID, sibling.ResourceVersion = "b", testRetentionSiblingUID, ""
				require.NoError(t, kube.Create(ctx, sibling))
			}
			changed := false
			boundary := interceptor.NewClient(kube, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
					return err
				}
				if changed || obj.GetName() != boundaryName {
					return nil
				}
				changed = true
				if err := kube.Get(ctx, client.ObjectKeyFromObject(process), process); err != nil {
					return err
				}
				process.Spec.Retain = true
				return kube.Update(ctx, process)
			}})
			require.NoError(t, (&executionRetentionService{Client: boundary}).reconcileTranscripts(ctx, ws, ws.Spec.ExecutionRetention))
			pods := new(corev1.PodList)
			require.NoError(t, kube.List(ctx, pods))
			if boundaryName == "a" {
				require.Empty(t, pods.Items)
			} else {
				require.Len(t, pods.Items, 1)
				var batch []processruntime.TranscriptIdentity
				require.NoError(t, json.Unmarshal([]byte(pods.Items[0].Spec.Containers[0].Args[0]), &batch))
				require.Equal(t, []processruntime.TranscriptIdentity{{ID: "b", UID: testRetentionSiblingUID}}, batch, "a pin during batch assembly excludes the early entry")
			}
			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(process), process))
			require.False(t, transcriptRequested(process))
			require.False(t, transcriptCleaned(process))
		})
	}
}

func TestDeletingPVCDischargesPendingBatch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	kube, ws, process := transcriptFixture(t)
	r := &executionRetentionService{Client: kube}
	require.NoError(t, r.reconcileTranscripts(ctx, ws, ws.Spec.ExecutionRetention))
	claim := new(corev1.PersistentVolumeClaim)
	require.NoError(t, kube.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: testTranscriptHomeClaim}, claim))
	claim.Finalizers = []string{testTranscriptHoldFinalizer}
	require.NoError(t, kube.Update(ctx, claim))
	require.NoError(t, kube.Delete(ctx, claim))
	require.NoError(t, r.reconcileTranscripts(ctx, ws, ws.Spec.ExecutionRetention))
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(process), process))
	require.True(t, transcriptCleaned(process))
	require.NoError(t, r.reconcileTranscripts(ctx, ws, ws.Spec.ExecutionRetention))
	pods := new(corev1.PodList)
	require.NoError(t, kube.List(ctx, pods))
	require.Empty(t, pods.Items, "PVC teardown cancels the batch without scheduling another worker")
}
