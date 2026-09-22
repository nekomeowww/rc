package repositories

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// RepositorySyncReconciler owns one authenticated sync and its durable result.
// Admission precedes Job creation. Readiness and the result are persisted before
// releasing the parent, so subsequent clones cannot observe a partial reset.
type RepositorySyncReconciler struct {
	client.Client
	APIReader   client.Reader
	Scheme      *runtime.Scheme
	RunnerImage string
}

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositorysyncs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositorysyncs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositorysyncs/finalizers,verbs=update

// Reconcile captures configuration only after acquiring the parent reservation.
//
//nolint:gocyclo // The request lifecycle keeps admission, execution, and cleanup ordering together.
func (r *RepositorySyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Request status and Job existence must reflect completed API writes, not
	// informer delivery order. This preserves at-most-once execution.
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	request := new(repositoriesv1alpha1.RepositorySync)
	if err := reader.Get(ctx, req.NamespacedName, request); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	token := repositoryaccess.Token("sync", request)
	terminal := meta.FindStatusCondition(request.Status.Conditions, repositoriesv1alpha1.RepositorySyncConditionSucceeded)
	if !request.DeletionTimestamp.IsZero() || (terminal != nil && terminal.Status != metav1.ConditionUnknown) {
		if !request.DeletionTimestamp.IsZero() && request.Status.JobName != "" && (terminal == nil || terminal.Status == metav1.ConditionUnknown) {
			repository := new(repositoriesv1alpha1.Repository)
			err := r.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: request.Spec.RepositoryRef.Name}, repository)
			if err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			if err == nil {
				if err := r.setParent(ctx, request, repository, metav1.ConditionFalse, repositorySyncFailed, "Sync request was deleted", nil); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
		done, err := releaseRepositoryOperation(ctx, r.Client, r.APIReader, request, token, request.Status.JobName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}

		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(request, repositoryOperationFinalizer) {
		controllerutil.AddFinalizer(request, repositoryOperationFinalizer)
		if err := r.Update(ctx, request); err != nil {
			return ctrl.Result{}, err
		}
	}
	repository := new(repositoriesv1alpha1.Repository)
	if err := r.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: request.Spec.RepositoryRef.Name}, repository); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, r.setResult(ctx, request, metav1.ConditionFalse, "RepositoryNotFound", "Referenced Repository does not exist", nil)
		}
		return ctrl.Result{}, err
	}
	job, state, err := observeOneShotJob(ctx, reader, request, request.Status.JobName)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch state {
	case oneShotJobPresent:
		if !jobFinished(job) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return ctrl.Result{}, r.complete(ctx, request, repository, job)
	case oneShotJobLost:
		if err := r.setParent(ctx, request, repository, metav1.ConditionFalse, repositorySyncFailed, "Sync Job disappeared before its result was recorded", nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.setResult(ctx, request, metav1.ConditionFalse, "JobLost", "Sync Job disappeared before its result was recorded", nil)
	case oneShotJobConflict:
		return ctrl.Result{}, r.setResult(ctx, request, metav1.ConditionFalse, "JobConflict", "A Job with this request name already exists", nil)
	}
	// Bootstrap must have completed for the current spec. A failed sync remains
	// retryable with a new request even though it made StorageReady false.
	ready := meta.FindStatusCondition(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady)
	if repository.Status.ObservedGeneration != repository.Generation || ready == nil || ready.ObservedGeneration != repository.Generation ||
		(ready.Status != metav1.ConditionTrue && ready.Reason != repositorySyncFailed) || repository.Status.VolumeClaimName == "" {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, r.setResult(ctx, request, metav1.ConditionUnknown, "RepositoryNotReady", "Waiting for Repository bootstrap", nil)
	}
	gate := repositoryaccess.Gate{Client: r.Client, Reader: r.APIReader}
	acquired, err := gate.Acquire(ctx, repository, token, repositoryaccess.Write, false)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !acquired {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, r.setResult(ctx, request, metav1.ConditionUnknown, "WaitingForRepository", "Waiting for parent writers, mounts, or clones", nil)
	}
	bootstrap := RepositoryReconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, RunnerImage: r.RunnerImage}
	credential, err := bootstrap.repositoryCredential(ctx, repository)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, r.setResult(ctx, request, metav1.ConditionFalse, "CredentialNotFound", "Referenced Credential does not exist", nil)
		}
		return ctrl.Result{}, err
	}
	job = repositoryBootstrapJob(repository, r.RunnerImage, credential)
	job.Name = request.Name
	// Retain the Job until the result has been recorded, even across controller outages.
	job.Spec.Template.Spec.Containers[0].Args[1] += "\ngit -C /repository rev-parse --verify HEAD > /dev/termination-log\n"
	if err := controllerutil.SetControllerReference(request, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	request.Status.JobName = job.Name
	request.Status.RepositoryGeneration = repository.Generation
	request.Status.RepositoryUID = string(repository.UID)
	if err := r.setResult(ctx, request, metav1.ConditionUnknown, "JobScheduled", "Sync Job is scheduled for creation", nil); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.setParent(ctx, request, repository, metav1.ConditionFalse, "SyncRunning", "Repository synchronization is running", nil); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil {
		return ctrl.Result{}, fmt.Errorf("create Repository sync Job: %w", err)
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

const repositorySyncFailed = "SyncFailed"

var resolvedCommitPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

func (r *RepositorySyncReconciler) complete(ctx context.Context, request *repositoriesv1alpha1.RepositorySync, repository *repositoriesv1alpha1.Repository, job *batchv1.Job) error {
	status, reason, message, _ := terminalJobOutcome(job)
	if status == metav1.ConditionTrue {
		pods := new(corev1.PodList)
		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}
		if err := reader.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{batchv1.JobNameLabel: job.Name}); err != nil {
			return err
		}
		for _, pod := range pods.Items {
			if !metav1.IsControlledBy(&pod, job) {
				continue
			}
			for _, container := range pod.Status.ContainerStatuses {
				if end := container.State.Terminated; container.Name == job.Spec.Template.Spec.Containers[0].Name && end != nil && end.ExitCode == 0 {
					commit := strings.TrimSpace(end.Message)
					if resolvedCommitPattern.MatchString(commit) {
						request.Status.Commit = commit
					}
				}
			}
		}
		if request.Status.Commit == "" {
			status = metav1.ConditionFalse
			reason = "ResultUnavailable"
			message = "Sync completed but the resolved commit is unavailable"
		}
	}
	parentReason := repositorySyncFailed
	if status == metav1.ConditionTrue {
		parentReason = "RepositoryReady"
		reason = "SyncSucceeded"
		message = "Repository synchronization completed"
	}
	completed := job.Status.CompletionTime
	if completed == nil {
		now := metav1.Now()
		completed = &now
	}
	if err := r.setParent(ctx, request, repository, status, parentReason, message, completed); err != nil {
		return err
	}
	if err := r.setResult(ctx, request, status, reason, message, completed); err != nil {
		return err
	}

	return nil
}

func (r *RepositorySyncReconciler) setParent(ctx context.Context, request *repositoriesv1alpha1.RepositorySync, repository *repositoriesv1alpha1.Repository, status metav1.ConditionStatus, reason, message string, completed *metav1.Time) error {
	if string(repository.UID) != request.Status.RepositoryUID {
		return nil
	}
	captured := repository.DeepCopy()
	captured.Generation = request.Status.RepositoryGeneration
	bootstrap := RepositoryReconciler{Client: r.Client, APIReader: r.APIReader}
	if status != metav1.ConditionTrue {
		completed = nil
	}
	return bootstrap.setStorageReady(ctx, captured, status, reason, message, repository.Status.VolumeClaimName, completed)
}

func (r *RepositorySyncReconciler) setResult(ctx context.Context, request *repositoriesv1alpha1.RepositorySync, status metav1.ConditionStatus, reason, message string, completed *metav1.Time) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.RepositorySync)
		if err := r.Get(ctx, client.ObjectKeyFromObject(request), current); err != nil {
			return err
		}
		if condition := meta.FindStatusCondition(current.Status.Conditions, repositoriesv1alpha1.RepositorySyncConditionSucceeded); condition != nil && condition.Status != metav1.ConditionUnknown {
			return nil
		}
		before := current.DeepCopy()
		current.Status.JobName = request.Status.JobName
		current.Status.RepositoryGeneration = request.Status.RepositoryGeneration
		current.Status.RepositoryUID = request.Status.RepositoryUID
		current.Status.Commit = request.Status.Commit
		current.Status.CompletedAt = completed
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: repositoriesv1alpha1.RepositorySyncConditionSucceeded, Status: status, Reason: reason, Message: message, ObservedGeneration: current.Generation})
		if equality.Semantic.DeepEqual(before.Status, current.Status) {
			return nil
		}
		return r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}

// SetupWithManager watches Jobs and keeps admission reads outside the cache.
func (r *RepositorySyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RunnerImage == "" {
		return fmt.Errorf("repository sync runner image must not be empty")
	}
	r.APIReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).For(&repositoriesv1alpha1.RepositorySync{}).Owns(&batchv1.Job{}).Named("repositories-repositorysync").Complete(r)
}
