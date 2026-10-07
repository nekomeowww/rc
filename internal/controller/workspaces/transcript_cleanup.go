package workspaces

import (
	"context"
	"errors"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	transcriptCleanupCondition = workspacesv1alpha1.WorkspaceExecConditionTranscriptCleanup
	transcriptHomeVolumeName   = "home"
)

func transcriptCleaned(process *workspacesv1alpha1.WorkspaceExec) bool {
	return meta.IsStatusConditionTrue(process.Status.Conditions, transcriptCleanupCondition)
}

func transcriptRequested(process *workspacesv1alpha1.WorkspaceExec) bool {
	return meta.IsStatusConditionFalse(process.Status.Conditions, transcriptCleanupCondition)
}

func setTranscriptCondition(process *workspacesv1alpha1.WorkspaceExec, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&process.Status.Conditions, metav1.Condition{Type: transcriptCleanupCondition, Status: status, Reason: reason, Message: message, ObservedGeneration: process.Generation})
}

func (r *WorkspaceExecReconciler) ensureExecutionCompletedAt(ctx context.Context, process *workspacesv1alpha1.WorkspaceExec) error {
	if process.Status.CompletedAt != nil {
		return nil
	}
	// Legacy completion is unknown; start its age now, never at creation time.
	now := metav1.Now()
	process.Status.CompletedAt = &now
	return r.Status().Update(ctx, process)
}

// transcriptStorageGone ends per-transcript obligations during whole-target or
// PVC deletion. No worker can safely depend on storage that GC is dismantling.
func transcriptStorageGone(ctx context.Context, reader client.Reader, process *workspacesv1alpha1.WorkspaceExec) (bool, error) {
	target, err := readExecutionTarget(ctx, reader, process)
	if err != nil {
		return false, err
	}
	if target == nil || !target.GetDeletionTimestamp().IsZero() {
		return true, nil
	}
	if process.Status.TranscriptVolumeClaimName == "" {
		return false, nil
	}
	claim := new(corev1.PersistentVolumeClaim)
	err = reader.Get(ctx, client.ObjectKey{Namespace: process.Namespace, Name: process.Status.TranscriptVolumeClaimName}, claim)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !claim.DeletionTimestamp.IsZero() || (process.Status.TranscriptVolumeClaimUID != "" && string(claim.UID) != process.Status.TranscriptVolumeClaimUID), nil
}

// requestTranscriptCleanup leaves the storage obligation on the CR; the target
// service owns all filesystem work and worker creation. The one Condition is
// both the durable intent and acknowledgement, with no duplicate timestamps.
func (r *WorkspaceExecReconciler) requestTranscriptCleanup(ctx context.Context, process *workspacesv1alpha1.WorkspaceExec) (bool, error) {
	if transcriptCleaned(process) {
		return true, nil
	}
	gone, err := transcriptStorageGone(ctx, r.APIReader, process)
	if err != nil {
		return false, err
	}
	if gone || (process.Status.RuntimePodUID == "" && process.Status.TranscriptPath == "" && process.Status.TranscriptVolumeClaimName == "") {
		setTranscriptCondition(process, metav1.ConditionTrue, "StorageGone", "No individual transcript cleanup remains; target storage lifecycle applies")
		return true, r.Status().Update(ctx, process)
	}
	if !transcriptRequested(process) {
		setTranscriptCondition(process, metav1.ConditionFalse, "Requested", "Waiting for target transcript cleanup")
		return false, r.Status().Update(ctx, process)
	}
	return false, nil
}

// bindTranscriptVolume records the storage identity before Start may execute.
// The process name alone is insufficient after an Environment draft is promoted.
func (r *WorkspaceExecReconciler) bindTranscriptVolume(ctx context.Context, process *workspacesv1alpha1.WorkspaceExec, target *resolvedProcessTarget) error {
	if process.Status.TranscriptVolumeClaimName != "" {
		return nil
	}
	pod := new(corev1.Pod)
	if err := r.Get(ctx, client.ObjectKey{Namespace: process.Namespace, Name: target.runtime.Pod}, pod); err != nil {
		return err
	}
	if string(pod.UID) != target.podUID {
		return errors.New("runtime Pod changed before transcript binding")
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != transcriptHomeVolumeName || volume.PersistentVolumeClaim == nil {
			continue
		}
		claim := new(corev1.PersistentVolumeClaim)
		if err := r.Get(ctx, client.ObjectKey{Namespace: process.Namespace, Name: volume.PersistentVolumeClaim.ClaimName}, claim); err != nil {
			return err
		}
		process.Status.TranscriptVolumeClaimName = claim.Name
		process.Status.TranscriptVolumeClaimUID = string(claim.UID)
	}
	return nil
}
