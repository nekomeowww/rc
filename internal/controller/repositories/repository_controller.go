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
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/nekomeowww/rc/internal/volumeclaim"
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
	// APIReader is required. It bypasses the informer cache; SetupWithManager
	// sets it from the manager.
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
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees;repositorysyncs;repositoryexecs,verbs=get
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces,verbs=get

// Reconcile ensures that every Repository owns one persistent parent volume and
// that its configured remote is bootstrapped into that volume.
//
// The Repository's access mirror is published after every pass.
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	result, err := r.reconcileRepository(ctx, req)
	if err != nil {
		return result, err
	}
	return result, r.publishAccess(ctx, req.NamespacedName)
}

//nolint:gocyclo // reconcileRepository is an existing explicit resource lifecycle state machine.
func (r *RepositoryReconciler) reconcileRepository(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	repository := new(repositoriesv1alpha1.Repository)
	if err := r.Get(ctx, req.NamespacedName, repository); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !repository.DeletionTimestamp.IsZero() {
		return r.reconcileDeletionBlocked(ctx, repository)
	}
	// A reservation whose owner was force-deleted would block writers forever.
	if err := r.sweepRepositoryHolders(ctx, repository); err != nil {
		return ctrl.Result{}, err
	}
	gate := repositoryaccess.Gate{Client: r.Client, Reader: r.APIReader}
	holder := repositoryaccess.Holder(repositoryaccess.KindRepository, repository, repositoryaccess.Write)
	if busy, err := gate.Busy(ctx, repository, holder.Key()); err != nil {
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
	if err := r.APIReader.List(ctx, jobs, client.InNamespace(repository.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for _, previous := range jobs.Items {
		if metav1.IsControlledBy(&previous, repository) && previous.Name != repositoryBootstrapJobName(repository) && !jobFinished(&previous) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	claimName, claim, err := volumeclaim.Resolve(ctx, r.APIReader, repository, volumeclaim.Repository, 0, repository.Status.VolumeClaimName)
	if volumeclaim.IsConflict(err) {
		return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "VolumeClaimConflict", err.Error(), repository.Status.VolumeClaimName, nil)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get parent PersistentVolumeClaim: %w", err)
	}
	if claim == nil {
		claim = parentVolumeClaim(repository, claimName)

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

	// Persist recovery before admission: the Repository gate compares the
	// entire captured status with the API before allowing a checkout.
	if repository.Status.VolumeClaimName != claim.Name {
		return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "Provisioning", "Recovered existing parent volume", claim.Name, nil)
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
		if err := gate.Release(ctx, repository, holder.Key()); err != nil {
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
					admission, err := gate.Acquire(ctx, repository, holder, true)
					if err != nil {
						return ctrl.Result{}, err
					}
					if admission != repositoryaccess.Admitted {
						return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
					}
					if err := setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionTrue, "RepositoryReady", "Repository Git content is ready", claim.Name, completionTime); err != nil {
						return ctrl.Result{}, err
					}
					return ctrl.Result{}, gate.Release(ctx, repository, holder.Key())
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
		admission, err := gate.Acquire(ctx, repository, holder, false)
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
		admission, err := gate.Acquire(ctx, repository, holder, false)
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
		return ctrl.Result{}, gate.Release(ctx, repository, holder.Key())
	}
	if condition := jobCondition(job, batchv1.JobComplete); condition != nil && condition.Status == corev1.ConditionTrue {
		if err := setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionTrue, "RepositoryReady", "Repository Git content is ready", claim.Name, job.Status.CompletionTime); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, gate.Release(ctx, repository, holder.Key())
	}

	return ctrl.Result{}, setRepositoryStorageReady(ctx, r.Client, repository, metav1.ConditionFalse, "Initializing", "Repository bootstrap Job is running", claim.Name, nil)
}

// reconcileDeletionBlocked publishes why a deleting Repository is still
// present. rc holds no Repository finalizer: only Kubernetes garbage collection
// (foreground deletion) or foreign finalizers keep it. rc reports the waits it
// can observe: live Pods using the parent volume, then the owned parent PVC.
func (r *RepositoryReconciler) reconcileDeletionBlocked(ctx context.Context, repository *repositoriesv1alpha1.Repository) (ctrl.Result, error) {
	wait, err := r.repositoryDeletionWait(ctx, repository)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.setDeletionBlocked(ctx, repository, wait); err != nil {
		return ctrl.Result{}, err
	}
	if wait == nil {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *RepositoryReconciler) repositoryDeletionWait(ctx context.Context, repository *repositoriesv1alpha1.Repository) (*cleanupWait, error) {
	claimName := repository.Status.VolumeClaimName
	if claimName == "" {
		return nil, nil
	}
	pods := new(corev1.PodList)
	if err := r.APIReader.List(ctx, pods, client.InNamespace(repository.Namespace)); err != nil {
		return nil, fmt.Errorf("list Pods using deleting Repository volume: %w", err)
	}
	consumers := make([]string, 0)
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed && podUsesPersistentVolumeClaim(pod, claimName) {
			consumers = append(consumers, pod.Name)
		}
	}
	if len(consumers) > 0 {
		slices.Sort(consumers)
		return &cleanupWait{reason: repositoriesv1alpha1.DeletionBlockedReasonWaitingForPods, message: "Waiting for Pods using the Repository volume to stop: " + strings.Join(consumers, ", ")}, nil
	}
	claim := new(corev1.PersistentVolumeClaim)
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: repository.Namespace, Name: claimName}, claim)
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get deleting Repository PersistentVolumeClaim: %w", err)
	}
	if !metav1.IsControlledBy(claim, repository) {
		return nil, nil
	}
	return &cleanupWait{reason: repositoriesv1alpha1.DeletionBlockedReasonWaitingForVolume, message: fmt.Sprintf("Waiting for PersistentVolumeClaim %s to be deleted", claim.Name)}, nil
}

// setDeletionBlocked writes DeletionBlocked only while the Repository is
// deleting. A nil wait removes the condition.
func (r *RepositoryReconciler) setDeletionBlocked(ctx context.Context, repository *repositoriesv1alpha1.Repository, wait *cleanupWait) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Repository)
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(repository), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != repository.UID || current.DeletionTimestamp.IsZero() {
			return nil
		}
		before := current.DeepCopy()
		if wait == nil {
			meta.RemoveStatusCondition(&current.Status.Conditions, repositoriesv1alpha1.RepositoryConditionDeletionBlocked)
		} else {
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type: repositoriesv1alpha1.RepositoryConditionDeletionBlocked, Status: metav1.ConditionTrue,
				ObservedGeneration: current.Generation, Reason: wait.reason, Message: wait.message,
			})
		}
		if slices.Equal(current.Status.Conditions, before.Status.Conditions) {
			return nil
		}
		return client.IgnoreNotFound(r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
	})
}

func parentVolumeClaim(repository *repositoriesv1alpha1.Repository, claimName string) *corev1.PersistentVolumeClaim {
	filesystem := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
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
		// Consumers change the access Lease; it is owned, not controlled.
		Watches(&coordinationv1.Lease{}, handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &repositoriesv1alpha1.Repository{})).
		Named("repositories-repository").
		Complete(r)
}
