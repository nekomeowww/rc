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
	"crypto/sha256"
	"fmt"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
	"github.com/nekomeowww/rc/internal/runtimepolicy"
)

const (
	worktreeBootstrapJobSuffix = "-bootstrap"
	worktreeBootstrapPath      = "/repository/worktree"
	worktreeBootstrapJobTTL    = int32(3 * 24 * 60 * 60)
	worktreeRepositoryLabel    = "rc.ayaka.io/worktree-repository"
	worktreeUIDLabel           = "repositories.rc.ayaka.io/worktree-uid"
	worktreeManagedByLabel     = "app.kubernetes.io/managed-by"
	worktreeManagedByValue     = "rc"
	generatedWorkspaceLabel    = "workspaces.rc.ayaka.io/generated-for"
	worktreeRequeueDelay       = 2 * time.Second
)

// WorktreeReconciler reconciles an independent child volume and the native
// Git worktree created inside it.
type WorktreeReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	RunnerImage string
}

// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees/finalizers,verbs=update
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositories,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
func (r *WorktreeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	worktree := new(repositoriesv1alpha1.Worktree)
	if err := r.Get(ctx, req.NamespacedName, worktree); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	worktreePath := worktreePath(worktree)
	repository := new(repositoriesv1alpha1.Repository)
	repositoryKey := types.NamespacedName{
		Name:      worktree.Spec.RepositoryRef.Name,
		Namespace: worktree.Namespace,
	}

	err := r.Get(ctx, repositoryKey, repository)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "RepositoryNotFound", "Referenced Repository does not exist", "", "", worktreePath)
		}
		return ctrl.Result{}, fmt.Errorf("get Repository: %w", err)
	}

	ready := meta.FindStatusCondition(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || repository.Status.VolumeClaimName == "" {
		if err := r.setWorktreeStatus(ctx, worktree, metav1.ConditionUnknown, "RepositoryNotReady", "Referenced Repository parent volume is not ready", "", repository.Status.VolumeClaimName, worktreePath); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: worktreeRequeueDelay}, nil
	}

	storageClassName, size, accessModes := effectiveWorktreeStorage(worktree, repository)
	claim := new(corev1.PersistentVolumeClaim)
	claimKey := types.NamespacedName{Name: worktree.Name, Namespace: worktree.Namespace}

	err = r.Get(ctx, claimKey, claim)
	if errors.IsNotFound(err) {
		claim = worktreeVolumeClaim(worktree, repository.Status.VolumeClaimName, storageClassName, size, accessModes)

		err := controllerutil.SetControllerReference(worktree, claim, r.Scheme)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("set Worktree owner on PersistentVolumeClaim: %w", err)
		}

		err = r.Create(ctx, claim)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("create Worktree PersistentVolumeClaim: %w", err)
		}

		log.Info("Created Worktree PersistentVolumeClaim", "name", claim.Name, "repository", repository.Name)

		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "Provisioning", "Child volume is provisioning", claim.Name, repository.Status.VolumeClaimName, worktreePath)
	}

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Worktree PersistentVolumeClaim: %w", err)
	}
	if !metav1.IsControlledBy(claim, worktree) {
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "VolumeClaimConflict", "A PersistentVolumeClaim with the Worktree name already exists and is not owned by this Worktree", "", repository.Status.VolumeClaimName, worktreePath)
	}
	if !worktreeClaimMatches(claim, repository.Status.VolumeClaimName, storageClassName, size, accessModes) {
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "VolumeClaimSpecChanged", "Changing the Worktree child volume specification is not supported", claim.Name, repository.Status.VolumeClaimName, worktreePath)
	}
	if claim.Status.Phase != corev1.ClaimBound {
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "Provisioning", "Child volume is provisioning", claim.Name, repository.Status.VolumeClaimName, worktreePath)
	}
	if err := r.labelWorktreePods(ctx, worktree, claim.Name); err != nil {
		return ctrl.Result{}, err
	}

	job := new(batchv1.Job)
	jobKey := types.NamespacedName{Name: worktreeBootstrapJobName(worktree), Namespace: worktree.Namespace}

	err = r.Get(ctx, jobKey, job)
	if errors.IsNotFound(err) {
		job = worktreeBootstrapJob(worktree, claim.Name, r.RunnerImage)
		err := controllerutil.SetControllerReference(worktree, job, r.Scheme)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("set Worktree owner on bootstrap Job: %w", err)
		}

		err = r.Create(ctx, job)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("create Worktree bootstrap Job: %w", err)
		}

		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "Initializing", "Worktree bootstrap Job is running", claim.Name, repository.Status.VolumeClaimName, worktreePath)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Worktree bootstrap Job: %w", err)
	}
	if !metav1.IsControlledBy(job, worktree) {
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "BootstrapJobConflict", "A bootstrap Job with the expected name is not owned by this Worktree", claim.Name, repository.Status.VolumeClaimName, worktreePath)
	}
	if condition := jobCondition(job, batchv1.JobFailed); condition != nil && condition.Status == corev1.ConditionTrue {
		message := condition.Message
		if message == "" {
			message = "Worktree bootstrap Job failed"
		}

		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "BootstrapFailed", message, claim.Name, repository.Status.VolumeClaimName, worktreePath)
	}
	if condition := jobCondition(job, batchv1.JobComplete); condition != nil && condition.Status == corev1.ConditionTrue {
		message := "Child volume and native Git worktree are ready"
		if reusesRepositoryRoot(worktree) {
			message = "Child volume and isolated Git checkout are ready"
		}
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionTrue, "WorktreeReady", message, claim.Name, repository.Status.VolumeClaimName, worktreePath)
	}

	return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "Initializing", "Worktree bootstrap Job is running", claim.Name, repository.Status.VolumeClaimName, worktreePath)
}

func effectiveWorktreeStorage(worktree *repositoriesv1alpha1.Worktree, repository *repositoriesv1alpha1.Repository) (string, resource.Quantity, []corev1.PersistentVolumeAccessMode) {
	storageClassName := repository.Spec.Storage.StorageClassName
	size := repository.Spec.Storage.Size.DeepCopy()
	accessModes := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	if storage := worktree.Spec.Storage; storage != nil {
		if storage.StorageClassName != "" {
			storageClassName = storage.StorageClassName
		}
		if storage.Size != nil {
			size = storage.Size.DeepCopy()
		}
		if len(storage.AccessModes) > 0 {
			accessModes = append([]corev1.PersistentVolumeAccessMode(nil), storage.AccessModes...)
		}
	}

	return storageClassName, size, accessModes
}

func worktreeVolumeClaim(
	worktree *repositoriesv1alpha1.Worktree,
	sourceClaimName string,
	storageClassName string,
	size resource.Quantity,
	accessModes []corev1.PersistentVolumeAccessMode,
) *corev1.PersistentVolumeClaim {
	filesystem := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      worktree.Name,
			Namespace: worktree.Namespace,
			Labels: map[string]string{
				worktreeManagedByLabel:  worktreeManagedByValue,
				worktreeRepositoryLabel: worktree.Spec.RepositoryRef.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      append([]corev1.PersistentVolumeAccessMode(nil), accessModes...),
			StorageClassName: &storageClassName,
			VolumeMode:       &filesystem,
			DataSource: &corev1.TypedLocalObjectReference{
				Kind: "PersistentVolumeClaim",
				Name: sourceClaimName,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
}

func worktreeClaimMatches(claim *corev1.PersistentVolumeClaim, sourceClaimName string, storageClassName string, size resource.Quantity, accessModes []corev1.PersistentVolumeAccessMode) bool {
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != storageClassName {
		return false
	}
	if claim.Spec.DataSource == nil || claim.Spec.DataSource.Kind != "PersistentVolumeClaim" || claim.Spec.DataSource.Name != sourceClaimName {
		return false
	}
	if !reflect.DeepEqual(claim.Spec.AccessModes, accessModes) {
		return false
	}

	currentSize, ok := claim.Spec.Resources.Requests[corev1.ResourceStorage]

	return ok && currentSize.Cmp(size) == 0
}

func podUsesPersistentVolumeClaim(pod *corev1.Pod, claimName string) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claimName {
			return true
		}
	}

	return false
}

func (r *WorktreeReconciler) labelWorktreePods(ctx context.Context, worktree *repositoriesv1alpha1.Worktree, claimName string) error {
	if worktree.UID == "" {
		return nil
	}

	pods := new(corev1.PodList)
	if err := r.List(ctx, pods, client.InNamespace(worktree.Namespace)); err != nil {
		return fmt.Errorf("list Worktree workload Pods: %w", err)
	}

	worktreeUID := string(worktree.UID)
	log := logf.FromContext(ctx)
	for index := range pods.Items {
		pod := &pods.Items[index]
		if !podUsesPersistentVolumeClaim(pod, claimName) {
			continue
		}

		currentUID := pod.Labels[worktreeUIDLabel]
		if currentUID == worktreeUID {
			continue
		}
		if currentUID != "" {
			log.Info("Skipped Worktree Pod with conflicting association label", "pod", pod.Name, "label", currentUID, "expected", worktreeUID)
			continue
		}

		before := pod.DeepCopy()
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		pod.Labels[worktreeUIDLabel] = worktreeUID
		if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("label Worktree workload Pod %q: %w", pod.Name, err)
		}
	}

	return nil
}

func worktreePath(worktree *repositoriesv1alpha1.Worktree) string {
	if reusesRepositoryRoot(worktree) {
		return workerMountPath
	}
	return worktreeBootstrapPath + "/" + worktree.Name
}

func reusesRepositoryRoot(worktree *repositoriesv1alpha1.Worktree) bool {
	return worktree.Labels[generatedWorkspaceLabel] != "" &&
		worktree.Spec.Branch != "" &&
		worktree.Spec.ResetBranch == "" &&
		worktree.Spec.Ref == "" &&
		!worktree.Spec.Detach &&
		!worktree.Spec.Orphan &&
		!worktree.Spec.NoCheckout &&
		!worktree.Spec.Lock
}

func worktreeBootstrapJobName(worktree *repositoriesv1alpha1.Worktree) string {
	name := worktree.Name + worktreeBootstrapJobSuffix
	if len(name) <= 63 {
		return name
	}

	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(worktree.Name)))[:10]

	return worktree.Name[:63-len(worktreeBootstrapJobSuffix)-len(digest)-1] + "-" + digest + worktreeBootstrapJobSuffix
}

func worktreeBootstrapJob(worktree *repositoriesv1alpha1.Worktree, claimName, runnerImage string) *batchv1.Job {
	bootstrapScript := worktreeBootstrapScript
	gitArgs := []string{worktree.Spec.Branch}
	if reusesRepositoryRoot(worktree) {
		bootstrapScript = generatedWorktreeBootstrapScript
	} else {
		gitArgs = []string{"worktree", "add"}
		if worktree.Spec.Branch != "" {
			gitArgs = append(gitArgs, "-b", worktree.Spec.Branch)
		}
		if worktree.Spec.ResetBranch != "" {
			gitArgs = append(gitArgs, "-B", worktree.Spec.ResetBranch)
		}
		if worktree.Spec.Detach {
			gitArgs = append(gitArgs, "--detach")
		}
		if worktree.Spec.Orphan {
			gitArgs = append(gitArgs, "--orphan")
		}
		if worktree.Spec.NoCheckout {
			gitArgs = append(gitArgs, "--no-checkout")
		}
		if worktree.Spec.Lock {
			gitArgs = append(gitArgs, "--lock")
			if worktree.Spec.LockReason != "" {
				gitArgs = append(gitArgs, "--reason", worktree.Spec.LockReason)
			}
		}

		gitArgs = append(gitArgs, worktreePath(worktree))
		if worktree.Spec.Ref != "" {
			gitArgs = append(gitArgs, worktree.Spec.Ref)
		}
	}

	backoffLimit := int32(0)
	ttlSecondsAfterFinished := worktreeBootstrapJobTTL
	allowPrivilegeEscalation := false
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      worktreeBootstrapJobName(worktree),
			Namespace: worktree.Namespace,
			Labels: map[string]string{
				worktreeManagedByLabel:  worktreeManagedByValue,
				worktreeRepositoryLabel: worktree.Spec.RepositoryRef.Name,
				worktreeUIDLabel:        string(worktree.UID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{worktreeUIDLabel: string(worktree.UID)}},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: runtimepolicy.AgentPodSecurityContext(),
					Containers: []corev1.Container{{
						Name:       "bootstrap",
						Image:      runnerImage,
						Command:    []string{"sh"},
						Args:       append([]string{"-ceu", bootstrapScript, "worktree-bootstrap"}, gitArgs...),
						WorkingDir: "/repository",
						Env:        []corev1.EnvVar{{Name: "HOME", Value: "/tmp"}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPrivilegeEscalation,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCapabilitiesDrop}},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: workerVolumeName, MountPath: workerMountPath}},
					}},
					Volumes: []corev1.Volume{{
						Name: workerVolumeName,
						VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: claimName,
						}},
					}},
				},
			},
		},
	}
}

func (r *WorktreeReconciler) setWorktreeStatus(ctx context.Context, worktree *repositoriesv1alpha1.Worktree, status metav1.ConditionStatus, reason, message, claimName, sourceClaimName, path string) error {
	key := client.ObjectKeyFromObject(worktree)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Worktree)

		err := r.Get(ctx, key, current)
		if err != nil {
			return client.IgnoreNotFound(err)
		}

		before := current.DeepCopy()
		current.Status.ObservedGeneration = current.Generation
		current.Status.SourceVolumeClaimName = sourceClaimName
		current.Status.VolumeClaimName = claimName
		current.Status.WorktreePath = path
		if claimName == "" {
			current.Status.JobName = ""
		} else if current.Status.JobName == "" || status != metav1.ConditionTrue {
			current.Status.JobName = worktreeBootstrapJobName(current)
		}

		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               repositoriesv1alpha1.WorktreeConditionReady,
			Status:             status,
			ObservedGeneration: current.Generation,
			Reason:             reason,
			Message:            message,
		})
		if current.Status.ObservedGeneration == before.Status.ObservedGeneration &&
			current.Status.SourceVolumeClaimName == before.Status.SourceVolumeClaimName &&
			current.Status.VolumeClaimName == before.Status.VolumeClaimName &&
			current.Status.WorktreePath == before.Status.WorktreePath &&
			current.Status.JobName == before.Status.JobName &&
			conditionsEqual(current.Status.Conditions, before.Status.Conditions) {
			return nil
		}

		err = r.Status().Patch(ctx, current, client.MergeFrom(before))
		if err != nil {
			return fmt.Errorf("patch Worktree status: %w", err)
		}

		return nil
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorktreeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RunnerImage == "" {
		return fmt.Errorf("worktree runner image must not be empty")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&repositoriesv1alpha1.Worktree{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []ctrl.Request {
			pod, ok := object.(*corev1.Pod)
			if !ok {
				return nil
			}

			worktrees := new(repositoriesv1alpha1.WorktreeList)
			if err := r.List(ctx, worktrees, client.InNamespace(pod.Namespace)); err != nil {
				return nil
			}

			requests := make([]ctrl.Request, 0)
			for index := range worktrees.Items {
				worktree := &worktrees.Items[index]
				claimName := worktree.Status.VolumeClaimName
				if claimName == "" {
					claimName = worktree.Name
				}
				if podUsesPersistentVolumeClaim(pod, claimName) {
					requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
				}
			}

			return requests
		})).
		Watches(&repositoriesv1alpha1.Repository{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []ctrl.Request {
			worktrees := new(repositoriesv1alpha1.WorktreeList)
			if err := r.List(ctx, worktrees, client.InNamespace(object.GetNamespace())); err != nil {
				return nil
			}
			requests := make([]ctrl.Request, 0)
			for index := range worktrees.Items {
				worktree := &worktrees.Items[index]
				if worktree.Spec.RepositoryRef.Name == object.GetName() {
					requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
				}
			}
			return requests
		})).
		Named("repositories-worktree").
		Complete(r)
}

const worktreeBootstrapScript = `
git config --global --add safe.directory /repository
mkdir -p /repository/worktree
git -C /repository worktree prune
git -C /repository -c checkout.workers=8 -c checkout.thresholdForParallelism=100 "$@"
git -C /repository worktree list --porcelain
`

const generatedWorktreeBootstrapScript = `
git config --global --add safe.directory /repository
git -C /repository checkout -b "$1"
git -C /repository rev-parse --verify HEAD
`
