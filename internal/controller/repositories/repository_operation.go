package repositories

import (
	"context"
	"fmt"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const repositoryOperationFinalizer = "repositories.rc.ayaka.io/parent-access"

// releaseRepositoryOperation waits for consumers to stop before releasing the
// parent. A lost Job alone is not proof that its Pods have stopped. Deletion
// uses foreground propagation, then removes the operation's finalizer last.
func releaseRepositoryOperation(ctx context.Context, c client.Client, reader client.Reader, owner client.Object, token, jobName string) (bool, error) {
	if reader == nil {
		reader = c
	}
	if jobName != "" {
		job := new(batchv1.Job)
		err := reader.Get(ctx, client.ObjectKey{Namespace: owner.GetNamespace(), Name: jobName}, job)
		if err != nil && !errors.IsNotFound(err) {
			return false, err
		}
		if err == nil && metav1.IsControlledBy(job, owner) {
			if !owner.GetDeletionTimestamp().IsZero() {
				if job.DeletionTimestamp.IsZero() {
					policy := metav1.DeletePropagationForeground
					if err := c.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil && !errors.IsNotFound(err) {
						return false, err
					}
				}
				return false, nil
			}
			if !jobFinished(job) {
				return false, nil
			}
			if job.Spec.TTLSecondsAfterFinished == nil {
				job.Spec.TTLSecondsAfterFinished = new(execJobTTLSeconds)
				if err := c.Update(ctx, job); err != nil {
					return false, err
				}
			}
		}
		pods := new(corev1.PodList)
		if err := reader.List(ctx, pods, client.InNamespace(owner.GetNamespace()), client.MatchingLabels{batchv1.JobNameLabel: jobName}); err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				return false, nil
			}
		}
	}
	if err := (repositoryaccess.Gate{Client: c, Reader: reader}).Release(ctx, owner.GetNamespace(), token); err != nil {
		return false, err
	}
	if controllerutil.ContainsFinalizer(owner, repositoryOperationFinalizer) {
		before := owner.DeepCopyObject().(client.Object)
		controllerutil.RemoveFinalizer(owner, repositoryOperationFinalizer)
		if err := c.Patch(ctx, owner, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}
	return true, nil
}

// setRepositoryStorageReady applies an operation result only to the captured
// Repository UID and generation. A late result cannot update a newer spec.
func setRepositoryStorageReady(
	ctx context.Context,
	c client.Client,
	repository *repositoriesv1alpha1.Repository,
	status metav1.ConditionStatus,
	reason string,
	message string,
	claimName string,
	lastUpdatedAt *metav1.Time,
) error {
	key := client.ObjectKeyFromObject(repository)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Repository)

		err := c.Get(ctx, key, current)
		if err != nil {
			return client.IgnoreNotFound(err)
		}

		if current.UID != repository.UID || current.Generation != repository.Generation {
			return nil
		}
		before := current.DeepCopy()
		current.Status.ObservedGeneration = repository.Generation
		current.Status.VolumeClaimName = claimName
		if lastUpdatedAt != nil {
			current.Status.LastUpdatedAt = lastUpdatedAt.DeepCopy()
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               repositoriesv1alpha1.RepositoryConditionStorageReady,
			Status:             status,
			ObservedGeneration: current.Generation,
			Reason:             reason,
			Message:            message,
		})
		if current.Status.ObservedGeneration == before.Status.ObservedGeneration &&
			current.Status.VolumeClaimName == before.Status.VolumeClaimName &&
			equality.Semantic.DeepEqual(current.Status.LastUpdatedAt, before.Status.LastUpdatedAt) &&
			conditionsEqual(current.Status.Conditions, before.Status.Conditions) {
			return nil
		}

		err = c.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		if err != nil {
			return fmt.Errorf("patch Repository status: %w", err)
		}
		return nil
	})
}
