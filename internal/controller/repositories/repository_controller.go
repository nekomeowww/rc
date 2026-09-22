/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package repositories

import (
	"context"
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
)

const (
	repositoryBootstrapJobSuffix = "-bootstrap-"
	repositoryManagedByLabel     = "app.kubernetes.io/managed-by"
	repositoryManagedByValue     = "rc"
	repositoryNameLabel          = "rc.ayaka.io/repository"
)

// RepositoryReconciler reconciles a Repository object.
type RepositoryReconciler struct {
	client.Client
	APIReader   client.Reader
	Scheme      *runtime.Scheme
	RunnerImage string
}

// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=configs.rc.ayaka.io,resources=credentials,verbs=get
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// Reconcile ensures that every Repository owns one persistent parent volume and
// that its configured remote is bootstrapped into that volume.
//
//nolint:gocyclo // Reconcile is an existing explicit resource lifecycle state machine.
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	repository := new(repositoriesv1alpha1.Repository)
	if err := r.Get(ctx, req.NamespacedName, repository); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !repository.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	gate := repositoryaccess.Gate{Client: r.Client, Reader: r.APIReader}
	token := repositoryaccess.Token("bootstrap", repository)
	if busy, err := gate.Busy(ctx, repository, token); err != nil {
		return ctrl.Result{}, err
	} else if busy {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	// Sync owns readiness until its result is persisted. An old completed bootstrap
	// must not turn a failed or running sync back into StorageReady=True.
	if condition := meta.FindStatusCondition(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady); condition != nil &&
		condition.ObservedGeneration == repository.Generation && (condition.Reason == "SyncRunning" || condition.Reason == repositorySyncFailed) {
		return ctrl.Result{}, nil
	}
	jobs := new(batchv1.JobList)
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.List(ctx, jobs, client.InNamespace(repository.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for _, previous := range jobs.Items {
		if metav1.IsControlledBy(&previous, repository) && previous.Name != repositoryBootstrapJobName(repository) && !jobFinished(&previous) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	claim := new(corev1.PersistentVolumeClaim)
	claimKey := types.NamespacedName{Name: repository.Name, Namespace: repository.Namespace}
	err := r.Get(ctx, claimKey, claim)
	if errors.IsNotFound(err) {
		claim = parentVolumeClaim(repository)

		err := controllerutil.SetControllerReference(repository, claim, r.Scheme)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("set Repository owner on PersistentVolumeClaim: %w", err)
		}

		err = r.Create(ctx, claim)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("create parent PersistentVolumeClaim: %w", err)
		}

		log.Info("Created parent PersistentVolumeClaim", "name", claim.Name)
		return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "Provisioning", "Parent volume is provisioning", claim.Name, nil)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get parent PersistentVolumeClaim: %w", err)
	}
	if !metav1.IsControlledBy(claim, repository) {
		return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "VolumeClaimConflict", "A PersistentVolumeClaim with the Repository name already exists and is not owned by this Repository", "", nil)
	}

	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != repository.Spec.Storage.StorageClassName {
		return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "StorageClassChangeUnsupported", "Changing storageClassName on an existing Repository is not supported", claim.Name, nil)
	}

	currentSize := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	desiredSize := repository.Spec.Storage.Size

	switch desiredSize.Cmp(currentSize) {
	case -1:
		return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "VolumeShrinkUnsupported", "Shrinking a Repository volume is not supported", claim.Name, nil)
	case 1:
		claim.Spec.Resources.Requests[corev1.ResourceStorage] = desiredSize

		err := r.Update(ctx, claim)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("expand parent PersistentVolumeClaim: %w", err)
		}

		return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "Expanding", "Parent volume is expanding", claim.Name, nil)
	}

	if ready := meta.FindStatusCondition(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady); ready != nil &&
		ready.Status == metav1.ConditionTrue && repository.Status.ObservedGeneration == repository.Generation &&
		claim.Status.Phase == corev1.ClaimBound {
		if err := gate.Release(ctx, repository.Namespace, token); err != nil {
			return ctrl.Result{}, err
		}
		if repository.Status.LastUpdatedAt != nil {
			return ctrl.Result{}, nil
		}
		// Re-read a completed Job so an upgraded controller can backfill the
		// timestamp and a future sync Job can advance it without duplicate work.
		completedJob := new(batchv1.Job)
		jobKey := types.NamespacedName{Name: repositoryBootstrapJobName(repository), Namespace: repository.Namespace}

		err := r.Get(ctx, jobKey, completedJob)
		if err == nil {
			if completionTime := completedJob.Status.CompletionTime; completionTime != nil {
				if condition := jobCondition(completedJob, batchv1.JobComplete); condition != nil && condition.Status == corev1.ConditionTrue {
					admission, err := gate.Acquire(ctx, repository, token, repositoryaccess.Write, true)
					if err != nil {
						return ctrl.Result{}, err
					}
					if admission != repositoryaccess.Admitted {
						return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
					}
					if err := setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionTrue, "RepositoryReady", "Repository Git content is ready", claim.Name, completionTime); err != nil {
						return ctrl.Result{}, err
					}
					return ctrl.Result{}, gate.Release(ctx, repository.Namespace, token)
				}
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get completed Repository bootstrap Job: %w", err)
		}

		return ctrl.Result{}, nil
	}

	job := new(batchv1.Job)
	jobKey := types.NamespacedName{Name: repositoryBootstrapJobName(repository), Namespace: repository.Namespace}

	err = r.Get(ctx, jobKey, job)
	if errors.IsNotFound(err) {
		admission, err := gate.Acquire(ctx, repository, token, repositoryaccess.Write, false)
		if err != nil {
			return ctrl.Result{}, err
		}
		if admission != repositoryaccess.Admitted {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		credential, credentialErr := repositoryCredential(ctx, r.Client, repository)
		if credentialErr != nil {
			if errors.IsNotFound(credentialErr) {
				return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "CredentialNotFound", "Referenced Credential does not exist", claim.Name, nil)
			}

			return ctrl.Result{}, credentialErr
		}

		job = repositoryCheckoutJob(repository, repositoryBootstrapJobName(repository), r.RunnerImage, credential)
		if err := controllerutil.SetControllerReference(repository, job, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set Repository owner on bootstrap Job: %w", err)
		}
		if err := r.Create(ctx, job); err != nil {
			return ctrl.Result{}, fmt.Errorf("create Repository bootstrap Job: %w", err)
		}

		return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "Initializing", "Repository bootstrap Job is running", claim.Name, nil)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Repository bootstrap Job: %w", err)
	}

	if !metav1.IsControlledBy(job, repository) {
		return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "BootstrapJobConflict", "A bootstrap Job with the expected name is not owned by this Repository", claim.Name, nil)
	}
	if jobFinished(job) {
		admission, err := gate.Acquire(ctx, repository, token, repositoryaccess.Write, false)
		if err != nil {
			return ctrl.Result{}, err
		}
		if admission != repositoryaccess.Admitted {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}
	if condition := jobCondition(job, batchv1.JobFailed); condition != nil && condition.Status == corev1.ConditionTrue {
		message := condition.Message
		if message == "" {
			message = "Repository bootstrap Job failed"
		}
		if err := setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "BootstrapFailed", message, claim.Name, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, gate.Release(ctx, repository.Namespace, token)
	}
	if condition := jobCondition(job, batchv1.JobComplete); condition != nil && condition.Status == corev1.ConditionTrue {
		if err := setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionTrue, "RepositoryReady", "Repository Git content is ready", claim.Name, job.Status.CompletionTime); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, gate.Release(ctx, repository.Namespace, token)
	}

	return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "Initializing", "Repository bootstrap Job is running", claim.Name, nil)
}

func parentVolumeClaim(repository *repositoriesv1alpha1.Repository) *corev1.PersistentVolumeClaim {
	filesystem := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repository.Name,
			Namespace: repository.Namespace,
			Labels: map[string]string{
				repositoryManagedByLabel: repositoryManagedByValue,
				repositoryNameLabel:      repository.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			// TODO(repository-storage-options): Configurable access modes and
			// volume mode are deferred until a non-filesystem or multi-writer
			// Repository consumer is owner-approved.
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &repository.Spec.Storage.StorageClassName,
			VolumeMode:       &filesystem,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: repository.Spec.Storage.Size},
			},
		},
	}
}

func repositoryBootstrapJobName(repository *repositoriesv1alpha1.Repository) string {
	return repository.Name + repositoryBootstrapJobSuffix + strconv.FormatInt(repository.Generation, 10)
}

func jobCondition(job *batchv1.Job, conditionType batchv1.JobConditionType) *batchv1.JobCondition {
	for index := range job.Status.Conditions {
		condition := &job.Status.Conditions[index]
		if condition.Type == conditionType {
			return condition
		}
	}

	return nil
}

func conditionsEqual(left, right []metav1.Condition) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}

	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	if r.RunnerImage == "" {
		return fmt.Errorf("repository runner image must not be empty")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&repositoriesv1alpha1.Repository{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Named("repositories-repository").
		Complete(r)
}
