package workspaces

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/rcplatform"
	workspaceservice "github.com/nekomeowww/rc/internal/workspaces"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const transcriptClaimUIDAnnotation = "workspaces.rc.ayaka.io/transcript-claim-uid"

// transcriptCleanupName provides one durable worker slot per stable target,
// independent of execution count and the number of historical draft PVCs.
func transcriptCleanupName(target client.Object) string {
	sum := sha256.Sum256([]byte(string(target.GetUID()) + "/" + string(executionTargetReference(target).Kind)))
	return fmt.Sprintf("transcript-cleanup-%x", sum[:12])
}

func (r *executionRetentionService) createTranscriptWorker(ctx context.Context, target client.Object, volume workspaceservice.TranscriptVolume, claimUID string, batch []processruntime.TranscriptIdentity) error {
	// Recheck whole-target/volume lifecycle at the dispatch boundary. GC owns
	// cancellation if deletion races after this read and before Pod creation.
	current := target.DeepCopyObject().(client.Object)
	if err := r.retentionReader().Get(ctx, client.ObjectKeyFromObject(target), current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if current.GetUID() != target.GetUID() || !current.GetDeletionTimestamp().IsZero() {
		return nil
	}
	if volume.Claim == "" {
		return errors.New("offline transcript cleanup requires a PVC")
	}
	claim := new(corev1.PersistentVolumeClaim)
	if err := r.retentionReader().Get(ctx, client.ObjectKey{Namespace: target.GetNamespace(), Name: volume.Claim}, claim); err != nil {
		return client.IgnoreNotFound(err)
	}
	if string(claim.UID) != claimUID || !claim.DeletionTimestamp.IsZero() {
		return nil
	}
	// Batch assembly can span several API round trips. Re-read early entries
	// before dispatch so pins or identity changes during assembly are respected.
	eligible := make([]processruntime.TranscriptIdentity, 0, len(batch))
	for _, entry := range batch {
		snapshot := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: entry.ID, Namespace: target.GetNamespace(), UID: types.UID(entry.UID)}, Spec: workspacesv1alpha1.WorkspaceExecSpec{TargetRef: executionTargetReference(target)}}
		process, err := r.transcriptCandidate(ctx, snapshot)
		if err != nil {
			return err
		}
		if process == nil {
			continue
		}
		if process.Status.TranscriptVolumeClaimName != "" && (process.Status.TranscriptVolumeClaimName != volume.Claim || process.Status.TranscriptVolumeClaimUID != "" && process.Status.TranscriptVolumeClaimUID != claimUID) {
			continue
		}
		eligible = append(eligible, entry)
	}
	if len(eligible) == 0 {
		return nil
	}
	pod, err := volume.Runtime.TranscriptCleanupPod(rcplatform.TranscriptPodIntent{
		Metadata: metav1.ObjectMeta{Name: transcriptCleanupName(target), Namespace: target.GetNamespace(), Annotations: map[string]string{transcriptClaimUIDAnnotation: claimUID}},
		Image:    volume.Image, HomeClaim: volume.Claim,
	}, eligible)
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(target, pod, r.Scheme()); err != nil {
		return err
	}
	// A stopped Pod may retain an RWO attachment. Reuse its node until detach
	// completes, preserving OS/placement from the ordinary platform builder.
	pods := new(corev1.PodList)
	label := workspaceManagedByLabel
	if executionTargetReference(target).Kind == workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment {
		label = environmentManagedByLabel
	}
	if err := r.retentionReader().List(ctx, pods, client.InNamespace(target.GetNamespace()), client.MatchingLabels{label: target.GetName()}); err != nil {
		return err
	}
	for _, existing := range pods.Items {
		if !metav1.IsControlledBy(&existing, target) {
			continue
		}
		for _, mount := range existing.Spec.Volumes {
			if mount.PersistentVolumeClaim == nil || mount.PersistentVolumeClaim.ClaimName != volume.Claim {
				continue
			}
			if existing.Status.Phase != corev1.PodFailed && existing.Status.Phase != corev1.PodSucceeded {
				return fmt.Errorf("waiting for mounted runtime %s before offline cleanup", existing.Name)
			}
			pod.Spec.NodeName = existing.Spec.NodeName
		}
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// reconcileTranscriptWorker acknowledges every batch entry before removing the
// completed Pod. It never adopts a worker with a different target or PVC UID.
func (r *executionRetentionService) reconcileTranscriptWorker(ctx context.Context, target client.Object) (bool, error) {
	pod := new(corev1.Pod)
	key := client.ObjectKey{Namespace: target.GetNamespace(), Name: transcriptCleanupName(target)}
	if err := r.retentionReader().Get(ctx, key, pod); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(pod, target) {
		return true, errors.New("transcript worker has a different target identity")
	}
	if !pod.DeletionTimestamp.IsZero() {
		return true, nil
	}
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.Containers[0].Args) != 1 || len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].PersistentVolumeClaim == nil {
		return true, errors.New("invalid transcript worker batch")
	}
	var batch []processruntime.TranscriptIdentity
	if err := json.Unmarshal([]byte(pod.Spec.Containers[0].Args[0]), &batch); err != nil {
		return true, err
	}
	claim := new(corev1.PersistentVolumeClaim)
	err := r.retentionReader().Get(ctx, client.ObjectKey{Namespace: target.GetNamespace(), Name: pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName}, claim)
	gone := apierrors.IsNotFound(err)
	if err != nil && !gone {
		return true, err
	}
	gone = gone || !claim.DeletionTimestamp.IsZero() || string(claim.UID) != pod.Annotations[transcriptClaimUIDAnnotation]
	if !gone && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return true, nil
	}
	var failures []error
	var cleanupErr error
	if !gone && pod.Status.Phase == corev1.PodFailed {
		cleanupErr = fmt.Errorf("transcript cleanup Pod failed: %s", pod.Status.Message)
	}
	for _, entry := range batch {
		process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Namespace: target.GetNamespace(), Name: entry.ID, UID: types.UID(entry.UID)}}
		if err := r.ackTranscript(ctx, process, cleanupErr); err != nil {
			failures = append(failures, err)
		}
	}
	// Failed batches can be replayed; successful batches must survive lost acks.
	if cleanupErr == nil && len(failures) > 0 {
		return true, errors.Join(failures...)
	}
	uid, version := pod.UID, pod.ResourceVersion
	err = client.IgnoreNotFound(r.Delete(ctx, pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}))
	return true, errors.Join(cleanupErr, err, errors.Join(failures...))
}
