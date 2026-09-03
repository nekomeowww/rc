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
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/runtimepolicy"
	"github.com/nekomeowww/rc/internal/worktreebootstrap"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
)

const worktreeExecDependencyRequeue = 2 * time.Second

var worktreeExecStatus = oneShotStatusAdapter[*repositoriesv1alpha1.WorktreeExec]{
	newObject: func() *repositoriesv1alpha1.WorktreeExec { return new(repositoriesv1alpha1.WorktreeExec) },
	status: func(exec *repositoriesv1alpha1.WorktreeExec) (string, []metav1.Condition) {
		return exec.Status.JobName, exec.Status.Conditions
	},
	apply: func(exec *repositoriesv1alpha1.WorktreeExec, jobName string, conditions []metav1.Condition) {
		exec.Status.JobName = jobName
		exec.Status.Conditions = conditions
	},
	conditionType: repositoriesv1alpha1.WorktreeExecConditionSucceeded,
	resourceKind:  "WorktreeExec",
}

// WorktreeExecReconciler reconciles a WorktreeExec object.
type WorktreeExecReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	RunnerImage string
}

// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktreeexecs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktreeexecs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktreeexecs/finalizers,verbs=update
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete

// Reconcile runs one exact argv while holding the Worktree's exclusive-write
// Lease. The same Lease is used by Workspace runtimes.
func (r *WorktreeExecReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	exec := new(repositoriesv1alpha1.WorktreeExec)
	if err := r.Get(ctx, req.NamespacedName, exec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	condition := meta.FindStatusCondition(exec.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded)
	if condition != nil && condition.Status != metav1.ConditionUnknown {
		return ctrl.Result{}, r.releaseClaim(ctx, exec)
	}
	if len(exec.Spec.Command) == 0 || exec.Spec.Command[0] == "" {
		return ctrl.Result{}, r.setSucceeded(ctx, exec, metav1.ConditionFalse, "InvalidCommand", "Command must contain a non-empty executable", "")
	}

	job, jobState, err := observeOneShotJob(ctx, r.Client, exec, exec.Status.JobName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("observe Worktree Exec Job: %w", err)
	}
	switch jobState {
	case oneShotJobPresent:
		if reason, message, stopping := worktreeExecStoppingFailure(condition); stopping {
			if job.DeletionTimestamp.IsZero() {
				return ctrl.Result{RequeueAfter: worktreeExecDependencyRequeue}, r.stopJobAndFail(ctx, exec, job, reason, message)
			}
			return ctrl.Result{RequeueAfter: worktreeExecDependencyRequeue}, nil
		}
		if jobFinished(job) {
			return ctrl.Result{}, r.reflectJobStatus(ctx, exec, job)
		}
		worktree, err := r.readyWorktree(ctx, exec)
		if err != nil {
			return ctrl.Result{RequeueAfter: worktreeExecDependencyRequeue}, r.stopJobAndFail(ctx, exec, job, "WorktreeUnavailable", err.Error())
		}
		acquired, err := r.acquireClaim(ctx, exec, worktree)
		if err != nil {
			return ctrl.Result{RequeueAfter: worktreeExecDependencyRequeue}, r.stopJobAndFail(ctx, exec, job, "WriteLeaseVerificationFailed", fmt.Sprintf("Worktree write Lease could not be verified: %v", err))
		}
		if !acquired {
			return ctrl.Result{RequeueAfter: worktreeExecDependencyRequeue}, r.stopJobAndFail(ctx, exec, job, "WriteLeaseLost", "Worktree write Lease is held by another writer")
		}
		return ctrl.Result{}, r.reflectJobStatus(ctx, exec, job)
	case oneShotJobLost:
		podsGone, err := r.stopLostJobPods(ctx, exec)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !podsGone {
			if err := r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "StoppingAfterJobLost", "Command Job disappeared; waiting for its Pods to stop", exec.Status.JobName); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: worktreeExecDependencyRequeue}, nil
		}
		if err := r.releaseClaim(ctx, exec); err != nil {
			return ctrl.Result{}, err
		}
		if reason, message, stopping := worktreeExecStoppingFailure(condition); stopping {
			return ctrl.Result{}, r.setSucceeded(ctx, exec, metav1.ConditionFalse, reason, message, exec.Status.JobName)
		}
		return ctrl.Result{}, r.setSucceeded(ctx, exec, metav1.ConditionFalse, "JobLost", "Command Job disappeared before its terminal result was recorded", exec.Status.JobName)
	case oneShotJobConflict:
		return ctrl.Result{}, r.setSucceeded(ctx, exec, metav1.ConditionFalse, "JobConflict", "A Job with the requested name already exists", "")
	}

	worktree, err := r.readyWorktree(ctx, exec)
	if err != nil {
		if errors.Is(err, errWorktreeNotReady) {
			if statusErr := r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "WorktreeNotReady", err.Error(), ""); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: worktreeExecDependencyRequeue}, nil
		}
		return ctrl.Result{}, r.setSucceeded(ctx, exec, metav1.ConditionFalse, "WorktreeUnavailable", err.Error(), "")
	}

	acquired, err := r.acquireClaim(ctx, exec, worktree)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !acquired {
		if err := r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "WaitingForWorktree", "Another runtime or exec is using the Worktree", ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: worktreeExecDependencyRequeue}, nil
	}

	job = worktreeExecJob(exec, worktree, r.RunnerImage)
	if err := controllerutil.SetControllerReference(exec, job, r.Scheme); err != nil {
		_ = r.releaseClaim(ctx, exec)
		return ctrl.Result{}, fmt.Errorf("set WorktreeExec owner on Job: %w", err)
	}
	if err := r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "JobScheduled", "Command Job is scheduled for creation", job.Name); err != nil {
		_ = r.releaseClaim(ctx, exec)
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil {
		// Creation errors can be ambiguous. Keep both the durable Job marker and
		// the Lease until the next observation proves whether the Job exists.
		return ctrl.Result{}, fmt.Errorf("create Worktree Exec Job: %w", err)
	}
	log.Info("Created Worktree Exec Job", "name", job.Name, "worktree", worktree.Name)
	return ctrl.Result{}, r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "JobCreated", "Command Job was created", job.Name)
}

func (r *WorktreeExecReconciler) stopLostJobPods(ctx context.Context, exec *repositoriesv1alpha1.WorktreeExec) (bool, error) {
	pods := new(corev1.PodList)
	if err := r.List(ctx, pods, client.InNamespace(exec.Namespace), client.MatchingLabels{"job-name": exec.Status.JobName}); err != nil {
		return false, fmt.Errorf("list Pods for lost Worktree Exec Job: %w", err)
	}
	if len(pods.Items) == 0 {
		return true, nil
	}
	foreground := metav1.DeletePropagationForeground
	for index := range pods.Items {
		pod := &pods.Items[index]
		if err := r.Delete(ctx, pod, &client.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("stop Pod %q for lost Worktree Exec Job: %w", pod.Name, err)
		}
	}
	return false, nil
}

func (r *WorktreeExecReconciler) stopJobAndFail(
	ctx context.Context,
	exec *repositoriesv1alpha1.WorktreeExec,
	job *batchv1.Job,
	reason string,
	message string,
) error {
	foreground := metav1.DeletePropagationForeground
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("stop Worktree Exec Job safely: %w", err)
	}
	return r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "StoppingAfter"+reason, message, job.Name)
}

func worktreeExecStoppingFailure(condition *metav1.Condition) (string, string, bool) {
	if condition == nil || condition.Status != metav1.ConditionUnknown {
		return "", "", false
	}
	switch condition.Reason {
	case "StoppingAfterJobLost":
		return "JobLost", condition.Message, true
	case "StoppingAfterWorktreeUnavailable":
		return "WorktreeUnavailable", condition.Message, true
	case "StoppingAfterWriteLeaseLost":
		return "WriteLeaseLost", condition.Message, true
	case "StoppingAfterWriteLeaseVerificationFailed":
		return "WriteLeaseVerificationFailed", condition.Message, true
	default:
		return "", "", false
	}
}

var errWorktreeNotReady = errors.New("referenced Worktree is not ready")

func (r *WorktreeExecReconciler) readyWorktree(ctx context.Context, exec *repositoriesv1alpha1.WorktreeExec) (*repositoriesv1alpha1.Worktree, error) {
	worktree := new(repositoriesv1alpha1.Worktree)
	key := types.NamespacedName{Name: exec.Spec.WorktreeRef.Name, Namespace: exec.Namespace}
	if err := r.Get(ctx, key, worktree); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("referenced Worktree does not exist")
		}
		return nil, fmt.Errorf("get Worktree: %w", err)
	}
	if !worktree.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("referenced Worktree is being deleted")
	}
	ready := meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
	if worktree.Status.ObservedGeneration < worktree.Generation || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration < worktree.Generation || worktree.Status.VolumeClaimName == "" {
		return nil, errWorktreeNotReady
	}

	return worktree, nil
}

func (r *WorktreeExecReconciler) acquireClaim(ctx context.Context, exec *repositoriesv1alpha1.WorktreeExec, worktree *repositoriesv1alpha1.Worktree) (bool, error) {
	holder := string(exec.UID)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: worktreeclaim.LeaseName(worktree), Namespace: exec.Namespace, Labels: map[string]string{worktreeclaim.HolderLabel: exec.Name}},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	if err := controllerutil.SetControllerReference(exec, lease, r.Scheme); err != nil {
		return false, fmt.Errorf("set WorktreeExec owner on Worktree write Lease: %w", err)
	}
	if err := r.Create(ctx, lease); err == nil {
		return true, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("create Worktree write Lease: %w", err)
	}
	current := new(coordinationv1.Lease)
	if err := r.Get(ctx, client.ObjectKeyFromObject(lease), current); err != nil {
		return false, fmt.Errorf("get Worktree write Lease: %w", err)
	}
	return current.Spec.HolderIdentity != nil && *current.Spec.HolderIdentity == holder, nil
}

func (r *WorktreeExecReconciler) releaseClaim(ctx context.Context, exec *repositoriesv1alpha1.WorktreeExec) error {
	leases := new(coordinationv1.LeaseList)
	if err := r.List(ctx, leases, client.InNamespace(exec.Namespace), client.MatchingLabels{worktreeclaim.HolderLabel: exec.Name}); err != nil {
		return fmt.Errorf("list WorktreeExec write Leases: %w", err)
	}
	for index := range leases.Items {
		lease := &leases.Items[index]
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(exec.UID) {
			continue
		}
		if err := r.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("release Worktree write Lease: %w", err)
		}
	}
	return nil
}

func worktreeExecJob(exec *repositoriesv1alpha1.WorktreeExec, worktree *repositoriesv1alpha1.Worktree, runnerImage string) *batchv1.Job {
	backoffLimit := int32(0)
	ttl := execJobTTLSeconds
	allowPrivilegeEscalation := false
	root := worktreebootstrap.VolumeRootMountPath(worktree.Name)
	workingDirectory := worktreebootstrap.NativeWorktreeMountPath(worktree.Name)
	if worktree.Status.WorktreePath == workerMountPath {
		workingDirectory = root
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: exec.Name, Namespace: exec.Namespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit, TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever, SecurityContext: runtimepolicy.AgentPodSecurityContext(),
				Containers: []corev1.Container{{
					Name: "exec", Image: runnerImage, Command: []string{exec.Spec.Command[0]}, Args: append([]string(nil), exec.Spec.Command[1:]...), WorkingDir: workingDirectory,
					SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivilegeEscalation, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{allCapabilitiesDrop}}},
					VolumeMounts:    []corev1.VolumeMount{{Name: workerVolumeName, MountPath: root}},
				}},
				Volumes: []corev1.Volume{{Name: workerVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: worktree.Status.VolumeClaimName}}}},
			}},
		},
	}
}

func (r *WorktreeExecReconciler) reflectJobStatus(ctx context.Context, exec *repositoriesv1alpha1.WorktreeExec, job *batchv1.Job) error {
	if status, reason, message, terminal := terminalJobOutcome(job); terminal {
		if err := r.releaseClaim(ctx, exec); err != nil {
			return err
		}
		return r.setSucceeded(ctx, exec, status, reason, message, job.Name)
	}
	return r.setSucceeded(ctx, exec, metav1.ConditionUnknown, "CommandRunning", "Command has not completed", job.Name)
}

func (r *WorktreeExecReconciler) setSucceeded(ctx context.Context, exec *repositoriesv1alpha1.WorktreeExec, status metav1.ConditionStatus, reason, message, jobName string) error {
	return worktreeExecStatus.set(ctx, r.Client, client.ObjectKeyFromObject(exec), status, reason, message, jobName)
}

func (r *WorktreeExecReconciler) execsForWorktree(ctx context.Context, object client.Object) []reconcile.Request {
	execs := new(repositoriesv1alpha1.WorktreeExecList)
	if err := r.List(ctx, execs, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for index := range execs.Items {
		exec := &execs.Items[index]
		if exec.Spec.WorktreeRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(exec)})
		}
	}
	return requests
}

func (r *WorktreeExecReconciler) execsForLease(ctx context.Context, object client.Object) []reconcile.Request {
	worktreeNames := worktreeNamesForLease(ctx, r.Client, object)
	if len(worktreeNames) == 0 {
		return nil
	}
	watched := make(map[string]struct{}, len(worktreeNames))
	for _, name := range worktreeNames {
		watched[name] = struct{}{}
	}
	execs := new(repositoriesv1alpha1.WorktreeExecList)
	if err := r.List(ctx, execs, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for index := range execs.Items {
		exec := &execs.Items[index]
		if _, exists := watched[exec.Spec.WorktreeRef.Name]; exists {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(exec)})
		}
	}
	return requests
}

func (r *WorktreeExecReconciler) execsForJobPod(ctx context.Context, object client.Object) []reconcile.Request {
	jobName := object.GetLabels()["job-name"]
	if jobName == "" {
		return nil
	}
	execs := new(repositoriesv1alpha1.WorktreeExecList)
	if err := r.List(ctx, execs, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, 1)
	for index := range execs.Items {
		exec := &execs.Items[index]
		if exec.Status.JobName == jobName {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(exec)})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorktreeExecReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RunnerImage == "" {
		return fmt.Errorf("worktree exec runner image must not be empty")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&repositoriesv1alpha1.WorktreeExec{}).
		Owns(&batchv1.Job{}).
		Watches(&coordinationv1.Lease{}, handler.EnqueueRequestsFromMapFunc(r.execsForLease)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.execsForJobPod)).
		Watches(&repositoriesv1alpha1.Worktree{}, handler.EnqueueRequestsFromMapFunc(r.execsForWorktree)).
		Named("repositories-worktreeexec").
		Complete(r)
}
