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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/runtimepolicy"
)

const (
	execJobTTLSeconds   = int32(3 * 24 * 60 * 60)
	repositoryUIDLabel  = "repositories.rc.ayaka.io/repository-uid"
	workerVolumeName    = "repository"
	workerMountPath     = "/repository"
	allCapabilitiesDrop = "ALL"
)

var repositoryExecStatus = oneShotStatusAdapter[*repositoriesv1alpha1.RepositoryExec]{
	newObject: func() *repositoriesv1alpha1.RepositoryExec { return new(repositoriesv1alpha1.RepositoryExec) },
	status: func(exec *repositoriesv1alpha1.RepositoryExec) (string, []metav1.Condition) {
		return exec.Status.JobName, exec.Status.Conditions
	},
	apply: func(exec *repositoriesv1alpha1.RepositoryExec, jobName string, conditions []metav1.Condition) {
		exec.Status.JobName = jobName
		exec.Status.Conditions = conditions
	},
	conditionType: repositoriesv1alpha1.RepositoryExecConditionSucceeded,
	resourceKind:  "RepositoryExec",
}

// RepositoryExecReconciler reconciles a RepositoryExec object.
type RepositoryExecReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	RunnerImage string
}

// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositoryexecs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositoryexecs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositoryexecs/finalizers,verbs=update
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositories,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// Reconcile runs one exact argv against a Repository parent volume. Execs for
// the same Repository are serialized because the parent has one writer.
func (r *RepositoryExecReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	exec := new(repositoriesv1alpha1.RepositoryExec)

	err := r.Get(ctx, req.NamespacedName, exec)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if succeeded := meta.FindStatusCondition(exec.Status.Conditions, repositoriesv1alpha1.RepositoryExecConditionSucceeded); succeeded != nil && succeeded.Status != metav1.ConditionUnknown {
		return ctrl.Result{}, nil
	}

	job, jobState, err := observeOneShotJob(ctx, r.Client, exec, exec.Status.JobName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("observe Repository Exec Job: %w", err)
	}
	switch jobState {
	case oneShotJobPresent:
		return ctrl.Result{}, r.reflectJobStatus(ctx, exec, job)
	case oneShotJobLost:
		return ctrl.Result{}, r.setSucceeded(ctx, exec, metav1.ConditionFalse, "JobLost", "Command Job disappeared before its terminal result was recorded", exec.Status.JobName)
	case oneShotJobConflict:
		return ctrl.Result{}, r.setSucceeded(ctx, exec, metav1.ConditionFalse, "JobConflict", "A Job with the requested name already exists", "")
	}

	repository := new(repositoriesv1alpha1.Repository)
	repositoryKey := types.NamespacedName{Name: exec.Spec.RepositoryRef.Name, Namespace: exec.Namespace}

	err = r.Get(ctx, repositoryKey, repository)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setSucceeded(ctx, exec, metav1.ConditionFalse, "RepositoryNotFound", "Referenced Repository does not exist", "")
		}

		return ctrl.Result{}, fmt.Errorf("get Repository: %w", err)
	}

	ready := meta.FindStatusCondition(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady)
	if repository.Status.ObservedGeneration < repository.Generation || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration < repository.Generation || repository.Status.VolumeClaimName == "" {
		err := r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "RepositoryNotReady", "Referenced Repository parent volume is not ready", "")
		if err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	busy, err := r.repositoryHasActiveExec(ctx, repository, exec)
	if err != nil {
		return ctrl.Result{}, err
	}
	if busy {
		err := r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "WaitingForRepository", "Another exec is using the Repository parent volume", "")
		if err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	job = repositoryExecJob(exec, repository, r.RunnerImage)
	if err := controllerutil.SetControllerReference(exec, job, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set RepositoryExec owner on Job: %w", err)
	}
	if err := r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "JobScheduled", "Command Job is scheduled for creation", job.Name); err != nil {
		return ctrl.Result{}, err
	}

	err = r.Create(ctx, job)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("create Repository Exec Job: %w", err)
	}

	log.Info("Created Repository Exec Job", "name", job.Name, "repository", repository.Name)
	return ctrl.Result{}, r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "JobCreated", "Command Job was created", job.Name)
}

func (r *RepositoryExecReconciler) repositoryHasActiveExec(
	ctx context.Context,
	repository *repositoriesv1alpha1.Repository,
	current *repositoriesv1alpha1.RepositoryExec,
) (bool, error) {
	jobs := new(batchv1.JobList)

	err := r.List(ctx, jobs, client.InNamespace(repository.Namespace), client.MatchingLabels{
		repositoryUIDLabel: string(repository.UID),
	})
	if err != nil {
		return false, fmt.Errorf("list Repository Exec Jobs: %w", err)
	}

	for index := range jobs.Items {
		job := &jobs.Items[index]
		if job.Name != current.Name && !jobFinished(job) {
			return true, nil
		}
	}

	return false, nil
}

func repositoryExecJob(
	exec *repositoriesv1alpha1.RepositoryExec,
	repository *repositoriesv1alpha1.Repository,
	runnerImage string,
) *batchv1.Job {
	backoffLimit := int32(0)
	ttlSecondsAfterFinished := execJobTTLSeconds
	allowPrivilegeEscalation := false
	command := []string{exec.Spec.Command[0]}
	args := append([]string(nil), exec.Spec.Command[1:]...)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      exec.Name,
			Namespace: exec.Namespace,
			Labels: map[string]string{
				repositoryManagedByLabel: repositoryManagedByValue,
				repositoryUIDLabel:       string(repository.UID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{repositoryUIDLabel: string(repository.UID)}},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: runtimepolicy.AgentPodSecurityContext(),
					Containers: []corev1.Container{
						{
							// TODO(repository-exec-credentials): Repository credentials are
							// intentionally not mounted into arbitrary exec commands. Add an
							// owner-approved, non-exfiltrating credential boundary before
							// Repository Sync consumes Credential resources.
							Name:       "exec",
							Image:      runnerImage,
							Command:    command,
							Args:       args,
							WorkingDir: workerMountPath,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: &allowPrivilegeEscalation,
								Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCapabilitiesDrop}},
							},
							VolumeMounts: []corev1.VolumeMount{{Name: workerVolumeName, MountPath: workerMountPath}},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: workerVolumeName,
							VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: repository.Status.VolumeClaimName,
							}},
						},
					},
				},
			},
		},
	}
}

func (r *RepositoryExecReconciler) reflectJobStatus(
	ctx context.Context,
	exec *repositoriesv1alpha1.RepositoryExec,
	job *batchv1.Job,
) error {
	if status, reason, message, terminal := terminalJobOutcome(job); terminal {
		return r.setSucceeded(ctx, exec, status, reason, message, job.Name)
	}

	return r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "CommandRunning", "Command has not completed", job.Name)
}

func (r *RepositoryExecReconciler) setSucceeded(
	ctx context.Context,
	exec *repositoriesv1alpha1.RepositoryExec,
	status metav1.ConditionStatus,
	reason string,
	message string,
	jobName string,
) error {
	return repositoryExecStatus.set(ctx, r.Client, client.ObjectKeyFromObject(exec), status, reason, message, jobName)
}

func jobFinished(job *batchv1.Job) bool {
	_, _, _, terminal := terminalJobOutcome(job)
	return terminal
}

// SetupWithManager sets up the controller with the Manager.
func (r *RepositoryExecReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RunnerImage == "" {
		return fmt.Errorf("repository exec runner image must not be empty")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&repositoriesv1alpha1.RepositoryExec{}).
		Owns(&batchv1.Job{}).
		Named("repositories-repositoryexec").
		Complete(r)
}
