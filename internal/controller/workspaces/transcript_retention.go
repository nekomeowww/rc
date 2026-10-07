package workspaces

import (
	"context"
	"errors"
	"fmt"
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/executionretention"
	"github.com/nekomeowww/rc/internal/rcplatform"
	workspaceservice "github.com/nekomeowww/rc/internal/workspaces"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Storage teardown discharges cleanup without mounting a deleting claim.
var errTranscriptStorageGone = errors.New("original transcript PVC is deleting")

// transcriptExpired is deliberately independent of CR age/count planning.
func transcriptExpired(process *workspacesv1alpha1.WorkspaceExec, policy *workspacesv1alpha1.ExecutionRetentionPolicy, now time.Time) bool {
	if policy == nil || process.Spec.Retain || !process.Status.Phase.Terminal() || process.Status.CompletedAt == nil || transcriptCleaned(process) {
		return false
	}
	ttl := executionretention.DefaultTranscriptTTL
	if policy.TranscriptTTL != nil {
		ttl = policy.TranscriptTTL.Duration
	}
	return !now.Before(process.Status.CompletedAt.Add(ttl))
}

// reconcileTranscripts drains the target's durable worker before building the
// next bounded batch. Offline PVCs are serialized under one target-owned Pod.
func (r *executionRetentionService) reconcileTranscripts(ctx context.Context, target client.Object, policy *workspacesv1alpha1.ExecutionRetentionPolicy) error {
	current := target.DeepCopyObject().(client.Object)
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(target), current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if current.GetUID() != target.GetUID() || !current.GetDeletionTimestamp().IsZero() {
		return nil
	}
	if busy, err := r.reconcileTranscriptWorker(ctx, current); busy || err != nil {
		return err
	}
	executions, err := r.executionHistory(ctx, target.GetNamespace(), executionTargetReference(target))
	if err != nil {
		return err
	}
	var batch []processruntime.TranscriptIdentity
	var volume workspaceservice.TranscriptVolume
	var claimUID string
	var failures []error
	attempts := 0
	for i := range executions {
		snapshot := &executions[i]
		if transcriptCleaned(snapshot) || (!transcriptRequested(snapshot) && !transcriptExpired(snapshot, policy, time.Now())) {
			continue
		}
		if attempts >= executionCleanupBatch {
			break
		}
		attempts++
		process, err := r.transcriptCandidate(ctx, snapshot)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if process == nil {
			continue
		}
		resolved, uid, err := r.cleanupVolume(ctx, process)
		if apierrors.IsNotFound(err) || errors.Is(err, errTranscriptStorageGone) {
			failures = append(failures, r.ackTranscript(ctx, process, nil))
			continue
		}
		if err != nil {
			failures = append(failures, r.ackTranscript(ctx, process, err))
			continue
		}
		if resolved.Claim == "" {
			// Darwin uses a node-local home and requires the original live supervisor.
			if process.Status.RuntimePodUID == "" {
				failures = append(failures, errors.New("transcript has no original volume or runtime"))
				continue
			}
		}
		handled, err := r.pruneLiveTranscript(ctx, current, process, resolved)
		if handled || err != nil {
			failures = append(failures, r.ackTranscript(ctx, process, err))
			continue
		}
		if len(batch) > 0 && (volume.Claim != resolved.Claim || claimUID != uid) {
			continue
		}
		volume, claimUID = resolved, uid
		batch = append(batch, processruntime.TranscriptIdentity{ID: process.Name, UID: string(process.UID)})
	}
	if len(batch) > 0 {
		failures = append(failures, r.createTranscriptWorker(ctx, current, volume, claimUID, batch))
	}
	return errors.Join(failures...)
}

// transcriptCandidate revalidates policy/pins and ownership immediately before
// dispatch. Deleting active records enter only after their finalizer stopped them.
func (r *executionRetentionService) transcriptCandidate(ctx context.Context, snapshot *workspacesv1alpha1.WorkspaceExec) (*workspacesv1alpha1.WorkspaceExec, error) {
	process := new(workspacesv1alpha1.WorkspaceExec)
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot), process); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if process.UID != snapshot.UID || process.Spec.TargetRef != snapshot.Spec.TargetRef || transcriptCleaned(process) {
		return nil, nil
	}
	gone, err := transcriptStorageGone(ctx, r.APIReader, process)
	if err != nil {
		return nil, err
	}
	if gone {
		return nil, r.ackTranscript(ctx, process, nil)
	}
	if process.DeletionTimestamp.IsZero() {
		policy, err := r.currentExecutionPolicy(ctx, process)
		if err != nil {
			return nil, err
		}
		if !transcriptExpired(process, policy, time.Now()) {
			if transcriptRequested(process) {
				meta.RemoveStatusCondition(&process.Status.Conditions, transcriptCleanupCondition)
				return nil, r.Status().Update(ctx, process)
			}
			return nil, nil
		}
	} else if !transcriptRequested(process) {
		return nil, nil
	}
	if !transcriptRequested(process) {
		setTranscriptCondition(process, metav1.ConditionFalse, "Requested", "Waiting for target transcript cleanup")
		if err := r.Status().Update(ctx, process); err != nil {
			return nil, err
		}
		// Revalidate after persisting intent, as with legacy finalizer installation.
		return r.transcriptCandidate(ctx, process)
	}
	return process, nil
}

func (r *executionRetentionService) cleanupVolume(ctx context.Context, process *workspacesv1alpha1.WorkspaceExec) (workspaceservice.TranscriptVolume, string, error) {
	volume, err := workspaceservice.ResolveTranscriptVolume(ctx, r.APIReader, process)
	if err != nil || volume.Claim == "" {
		return volume, "", err
	}
	// A pinned claim was already read during resolution.
	if volume.ClaimUID != "" {
		if volume.ClaimDeleting {
			return volume, "", errTranscriptStorageGone
		}
		return volume, volume.ClaimUID, nil
	}
	claim := new(corev1.PersistentVolumeClaim)
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: process.Namespace, Name: volume.Claim}, claim); err != nil {
		return volume, "", err
	}
	if !claim.DeletionTimestamp.IsZero() {
		return volume, "", errTranscriptStorageGone
	}
	return volume, string(claim.UID), nil
}

// pruneLiveTranscript keeps unlinking under the supervisor lock whenever a
// runtime still mounts the original home, avoiding races with active writers.
func (r *executionRetentionService) pruneLiveTranscript(ctx context.Context, target client.Object, process *workspacesv1alpha1.WorkspaceExec, volume workspaceservice.TranscriptVolume) (bool, error) {
	pods, err := r.targetPods(ctx, target)
	if err != nil {
		return false, err
	}
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		original := pod.Name == process.Status.RuntimePodName && string(pod.UID) == process.Status.RuntimePodUID
		if !original && (!metav1.IsControlledBy(pod, target) || !podMountsClaim(pod, volume.Claim, transcriptHomeVolumeName)) {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
			return false, fmt.Errorf("waiting for runtime %s to settle before transcript cleanup", pod.Name)
		}
		if r.Runtime == nil {
			return false, errors.New("runtime does not support transcript cleanup")
		}
		platform, err := rcplatform.FromPod(pod)
		if err != nil {
			return false, err
		}
		err = r.Runtime.PruneTranscript(ctx, platform.ProcessTarget(process.Namespace, pod.Name, runtimeContainerName), process.Name, string(process.UID))
		return err == nil, err
	}
	return false, nil
}

// ackTranscript records success only for the original CR identity. The worker
// survives any failed acknowledgement, so restart safely retries the same batch.
func (r *executionRetentionService) ackTranscript(ctx context.Context, process *workspacesv1alpha1.WorkspaceExec, cleanupErr error) error {
	current := new(workspacesv1alpha1.WorkspaceExec)
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(process), current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if current.UID != process.UID || transcriptCleaned(current) {
		return cleanupErr
	}
	status, reason, message := metav1.ConditionTrue, "CleanupComplete", "Transcript cleanup discharged by removal or storage loss; surviving ownership tombstones are retained"
	if cleanupErr != nil {
		status, reason, message = metav1.ConditionFalse, "CleanupFailed", cleanupErr.Error()
	}
	previous := meta.FindStatusCondition(current.Status.Conditions, transcriptCleanupCondition)
	if previous != nil && previous.Status == status && previous.Reason == reason && previous.Message == message {
		return cleanupErr
	}
	setTranscriptCondition(current, status, reason, message)
	return errors.Join(cleanupErr, r.Status().Update(ctx, current))
}
