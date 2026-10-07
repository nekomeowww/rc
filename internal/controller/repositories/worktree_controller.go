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
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/nekomeowww/rc/internal/runtimepolicy"
	"github.com/nekomeowww/rc/internal/volumeclaim"
	"github.com/nekomeowww/rc/internal/worktreebootstrap"
	"github.com/nekomeowww/rc/internal/worktreeownership"
	"github.com/nekomeowww/rc/internal/worktreestorage"
)

const (
	worktreeBootstrapJobSuffix = "-bootstrap"
	worktreeBootstrapJobTTL    = int32(3 * 24 * 60 * 60)
	worktreeRepositoryLabel    = "rc.ayaka.io/worktree-repository"
	worktreeUIDLabel           = "repositories.rc.ayaka.io/worktree-uid"
	worktreePathAnnotation     = "repositories.rc.ayaka.io/worktree-path"
	worktreeManagedByLabel     = "app.kubernetes.io/managed-by"
	worktreeManagedByValue     = "rc"
	worktreeRequeueDelay       = 2 * time.Second
	gitCheckoutSubcommand      = "checkout"
	worktreeDeletionFinalizer  = worktreeownership.DeletionFinalizer
)

// WorktreeReconciler reconciles an independent child volume and the Git
// checkout initialized in its cloned Repository root.
type WorktreeReconciler struct {
	client.Client
	// APIReader is required. It bypasses the informer cache; SetupWithManager
	// sets it from the manager.
	APIReader   client.Reader
	Scheme      *runtime.Scheme
	RunnerImage string
}

// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees/finalizers,verbs=update
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositories,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;delete
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktreeexecs,verbs=get;list;watch
//
//nolint:gocyclo // Reconcile is an explicit resource lifecycle state machine.
func (r *WorktreeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	// Both creation evidence and child existence must be read outside the cache;
	// a stale pre-creation Worktree must not authorize replacing a lost child.
	worktree := new(repositoriesv1alpha1.Worktree)
	if err := r.APIReader.Get(ctx, req.NamespacedName, worktree); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Migration and stale-holder repair run before deletion too: a holder
	// whose owner was force-deleted must not deadlock the finalizer.
	if changed, err := r.reconcileWorktreeHolders(ctx, worktree); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		if err := r.APIReader.Get(ctx, req.NamespacedName, worktree); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	if !worktree.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, worktree)
	}
	if !controllerutil.ContainsFinalizer(worktree, worktreeDeletionFinalizer) {
		before := worktree.DeepCopy()
		controllerutil.AddFinalizer(worktree, worktreeDeletionFinalizer)
		if err := r.Patch(ctx, worktree, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, fmt.Errorf("add Worktree deletion finalizer: %w", err)
		}
	}

	worktreePath := worktreePath(worktree)
	// Resolve reads directly before creation: a cached absence must not replan a
	// child already created from an earlier source observation. An ownership
	// conflict still returns the claim so a terminating owned PVC reaches cleanup.
	claimName, claim, err := volumeclaim.Resolve(ctx, r.APIReader, worktree, volumeclaim.Worktree, 0, worktree.Status.VolumeClaimName)
	if err != nil && !volumeclaim.IsConflict(err) {
		return ctrl.Result{}, fmt.Errorf("get Worktree PersistentVolumeClaim: %w", err)
	}
	// Handle storage deletion before Repository readiness or checkout work. The
	// parent may already be absent; that must not strand a PVC finalizer.
	if worktreeownership.MountsClosed(worktree) || (claim == nil && worktree.Status.VolumeClaimName != "") || (claim != nil && metav1.IsControlledBy(claim, worktree) && !claim.DeletionTimestamp.IsZero()) {
		return r.reconcileStorageDeletion(ctx, worktree)
	}
	if volumeclaim.IsConflict(err) {
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, repositoriesv1alpha1.WorktreeReasonVolumeClaimConflict, err.Error(), worktree.Status.VolumeClaimName, worktree.Status.SourceVolumeClaimName, worktreePath)
	}
	if claim == nil {
		// A recorded child is durable creation evidence. Its loss must not silently
		// authorize a new clone from today's Repository under the same Worktree.
		if worktree.Status.VolumeClaimName != "" {
			if err := r.releaseClone(ctx, worktree); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, repositoriesv1alpha1.WorktreeReasonVolumeClaimLost, "Previously created child PVC is missing; restore the child or recreate the Worktree explicitly", worktree.Status.VolumeClaimName, worktree.Status.SourceVolumeClaimName, worktreePath)
		}
		// Repository state is an input to creation only. An existing independent
		// child must recover even if its Repository was replaced or removed.
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

		admission, err := r.cloneGate().Acquire(ctx, repository, cloneHolder(worktree), true)
		if err != nil {
			return ctrl.Result{}, err
		}
		if admission != repositoryaccess.Admitted {
			return ctrl.Result{RequeueAfter: worktreeRequeueDelay}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionUnknown, "RepositoryNotReady", "Repository is busy or not ready for cloning", "", repository.Status.VolumeClaimName, worktreePath)
		}
		// Admission stabilizes rc-managed Repository mutations. Read the actual
		// source after admission instead of using Repository intent or cache state.
		source := new(corev1.PersistentVolumeClaim)
		sourceKey := types.NamespacedName{Name: repository.Status.VolumeClaimName, Namespace: worktree.Namespace}
		if err := r.APIReader.Get(ctx, sourceKey, source); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("get clone source PVC: %w", err)
			}
			source = nil
		}
		plan, planErr := worktreestorage.PlanClone(source, repository, worktree.Spec.Storage)
		if planErr != nil {
			// No child was created: rejected planning must not retain a reservation
			// that would prevent Repository sync or expansion from making progress.
			if err := r.releaseClone(ctx, worktree); err != nil {
				return ctrl.Result{}, err
			}
			status := metav1.ConditionFalse
			result := ctrl.Result{}
			if planErr.Retryable() {
				status = metav1.ConditionUnknown
				result.RequeueAfter = worktreeRequeueDelay
			}
			return result, r.setWorktreeStatus(ctx, worktree, status, planErr.Reason, planErr.Message, "", repository.Status.VolumeClaimName, worktreePath)
		}
		claim = worktreeVolumeClaim(worktree, claimName, source.Name, plan)

		err = controllerutil.SetControllerReference(worktree, claim, r.Scheme)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("set Worktree owner on PersistentVolumeClaim: %w", err)
		}

		err = r.Create(ctx, claim)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("create Worktree PersistentVolumeClaim: %w", err)
		}

		log.Info("Created Worktree PersistentVolumeClaim", "name", claim.Name, "repository", repository.Name)

		// Creation and restart recovery publish status through the same path below.
		// The successful PVC write, not a later status write, commits clone identity.
	}

	// Existing child storage records its creation-time defaults. Do not compare
	// it to a fresh plan: parent expansion and default changes cannot alter a clone.
	// Worktree status is a repairable projection: creation may have committed
	// before status, and an older controller may have recorded the wrong source.
	// Recover only from this owned child's immutable source references.
	sourceClaimName, sourceErr := worktreestorage.CloneSourceName(claim)
	if sourceErr != nil {
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "VolumeClaimSpecChanged", sourceErr.Error(), claim.Name, "", worktreePath)
	}
	if err := worktreeownership.EnsureVolumeProtection(ctx, r.Client, claim); err != nil {
		return ctrl.Result{}, err
	}
	if !worktreeClaimMatches(claim, worktree.Spec.Storage) {
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "VolumeClaimSpecChanged", "Changing the Worktree child volume specification is not supported", claim.Name, sourceClaimName, worktreePath)
	}
	if claim.Status.Phase != corev1.ClaimBound {
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "Provisioning", "Child volume is provisioning", claim.Name, sourceClaimName, worktreePath)
	}
	// A Bound CSI clone is an independent volume. Pending claims retain admission
	// through source capture. See https://kubernetes.io/docs/concepts/storage/volume-pvc-datasource/
	if err := r.releaseClone(ctx, worktree); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.setWorktreeVolumeReady(ctx, worktree, claim.Name, sourceClaimName, worktreePath); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.labelWorktreePods(ctx, worktree, claim.Name); err != nil {
		return ctrl.Result{}, err
	}
	if ready := meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady); ready != nil &&
		ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == worktree.Generation &&
		worktree.Status.ObservedGeneration == worktree.Generation {
		return ctrl.Result{}, nil
	}
	if worktreebootstrap.Deferred(worktree) {
		legacyJob := new(batchv1.Job)
		legacyKey := types.NamespacedName{Name: worktreeBootstrapJobName(worktree), Namespace: worktree.Namespace}
		if err := r.Get(ctx, legacyKey, legacyJob); errors.IsNotFound(err) {
			return ctrl.Result{}, r.reconcileWorkspaceBootstrap(ctx, worktree, claim.Name, sourceClaimName, worktreePath)
		} else if err != nil {
			return ctrl.Result{}, fmt.Errorf("get legacy generated Worktree bootstrap Job: %w", err)
		}
	}

	job := new(batchv1.Job)
	jobKey := types.NamespacedName{Name: worktreeBootstrapJobName(worktree), Namespace: worktree.Namespace}

	err = r.Get(ctx, jobKey, job)
	if errors.IsNotFound(err) {
		// A Worktree created by an older controller can have the legacy nested
		// path in status before its bootstrap Job exists. A new Job always uses
		// the cloned Repository root, so publish the matching path with it.
		worktreePath = workerMountPath
		job = worktreeBootstrapJob(worktree, claim.Name, r.RunnerImage)
		err := controllerutil.SetControllerReference(worktree, job, r.Scheme)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("set Worktree owner on bootstrap Job: %w", err)
		}

		err = r.Create(ctx, job)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("create Worktree bootstrap Job: %w", err)
		}

		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "Initializing", "Worktree bootstrap Job is running", claim.Name, sourceClaimName, worktreePath)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Worktree bootstrap Job: %w", err)
	}
	if !metav1.IsControlledBy(job, worktree) {
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "BootstrapJobConflict", "A bootstrap Job with the expected name is not owned by this Worktree", claim.Name, sourceClaimName, worktreePath)
	}
	if initializedPath := job.Annotations[worktreePathAnnotation]; initializedPath != "" {
		worktreePath = initializedPath
	}
	if condition := jobCondition(job, batchv1.JobFailed); condition != nil && condition.Status == corev1.ConditionTrue {
		message := condition.Message
		if message == "" {
			message = "Worktree bootstrap Job failed"
		}

		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "BootstrapFailed", message, claim.Name, sourceClaimName, worktreePath)
	}
	if condition := jobCondition(job, batchv1.JobComplete); condition != nil && condition.Status == corev1.ConditionTrue {
		return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionTrue, "WorktreeReady", "Child volume and isolated Git checkout are ready", claim.Name, sourceClaimName, worktreePath)
	}

	return ctrl.Result{}, r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "Initializing", "Worktree bootstrap Job is running", claim.Name, sourceClaimName, worktreePath)
}

func (r *WorktreeReconciler) reconcileDelete(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) (ctrl.Result, error) {
	// Once our cleanup is complete, leave remaining finalizers to their owners.
	if !controllerutil.ContainsFinalizer(worktree, worktreeDeletionFinalizer) {
		return ctrl.Result{}, nil
	}
	// Finish an admitted clone before releasing its source reservation. Deleting
	// a pending PVC can leave a CSI CreateVolume operation in flight.
	// Cleanup resolves the same identity as provisioning, including recovery
	// before status is persisted. Absence at the CR name does not prove its
	// selected PVC is gone.
	claimName, wait, err := r.prepareWorktreeCleanup(ctx, worktree)
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait != nil {
		return ctrl.Result{RequeueAfter: worktreeRequeueDelay}, r.setDeletionBlocked(ctx, worktree, wait)
	}
	claim := new(corev1.PersistentVolumeClaim)
	claimKey := client.ObjectKey{Namespace: worktree.Namespace, Name: claimName}
	if err := r.APIReader.Get(ctx, claimKey, claim); err == nil && metav1.IsControlledBy(claim, worktree) {
		if claim.DeletionTimestamp.IsZero() && claim.Status.Phase != corev1.ClaimBound {
			return ctrl.Result{RequeueAfter: worktreeRequeueDelay}, r.setDeletionBlocked(ctx, worktree, &cleanupWait{
				reason:  repositoriesv1alpha1.DeletionBlockedReasonWaitingForVolume,
				message: fmt.Sprintf("Waiting for PersistentVolumeClaim %s to finish provisioning", claim.Name),
			})
		}
		// Foreground GC has already cancelled a terminating claim. It cannot bind
		// now: release our guard, then wait for PVC/provisioner cleanup instead of
		// waiting forever for ClaimBound. Keep the source reservation until absent.
		if err := worktreeownership.ReleaseVolumeProtection(ctx, r.Client, claim); err != nil {
			return ctrl.Result{}, err
		}
		if !claim.DeletionTimestamp.IsZero() {
			return ctrl.Result{RequeueAfter: worktreeRequeueDelay}, r.setDeletionBlocked(ctx, worktree, &cleanupWait{
				reason:  repositoriesv1alpha1.DeletionBlockedReasonWaitingForVolume,
				message: fmt.Sprintf("Waiting for PersistentVolumeClaim %s to be deleted", claim.Name),
			})
		}
	} else if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// Remaining finalizers belong to others; do not leave a stale blocker.
	if meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionDeletionBlocked) != nil {
		if err := r.setDeletionBlocked(ctx, worktree, nil); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.releaseClone(ctx, worktree); err != nil {
		return ctrl.Result{}, err
	}
	key := client.ObjectKeyFromObject(worktree)
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Worktree)
		if err := r.APIReader.Get(ctx, key, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		before := current.DeepCopy()
		controllerutil.RemoveFinalizer(current, worktreeDeletionFinalizer)
		// Foreground GC can complete deletion after the GET; absence is success.
		return client.IgnoreNotFound(r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove Worktree deletion finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *WorktreeReconciler) worktreesForWorkspace(_ context.Context, object client.Object) []ctrl.Request {
	workspace, ok := object.(*workspacesv1alpha1.Workspace)
	if !ok {
		return nil
	}
	names := make(map[string]struct{})
	for _, mount := range workspace.Spec.Mounts {
		if mount.WorktreeRef != nil {
			names[mount.WorktreeRef.Name] = struct{}{}
		}
	}
	requests := make([]ctrl.Request, 0, len(names))
	for name := range names {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: workspace.Namespace}})
	}
	slices.SortFunc(requests, func(left, right ctrl.Request) int { return strings.Compare(left.Name, right.Name) })
	return requests
}

// worktreeForExec enqueues an exec's Worktree, so a deleted exec's holder is
// swept and a running one's legacy Lease is imported.
func worktreeForExec(_ context.Context, object client.Object) []ctrl.Request {
	exec, ok := object.(*repositoriesv1alpha1.WorktreeExec)
	if !ok || exec.Spec.WorktreeRef.Name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: exec.Namespace, Name: exec.Spec.WorktreeRef.Name}}}
}

func (r *WorktreeReconciler) reconcileWorkspaceBootstrap(ctx context.Context, worktree *repositoriesv1alpha1.Worktree, claimName, sourceClaimName, path string) error {
	if meta.IsStatusConditionTrue(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady) {
		return r.setWorktreeStatus(ctx, worktree, metav1.ConditionTrue, "WorktreeReady", "Child volume and isolated Git checkout are ready", claimName, sourceClaimName, path)
	}
	pods := new(corev1.PodList)
	if err := r.List(ctx, pods, client.InNamespace(worktree.Namespace)); err != nil {
		return fmt.Errorf("list Pods while observing Workspace Worktree initialization: %w", err)
	}
	containerName := worktreebootstrap.ContainerName(worktree.Namespace, worktree.Name, worktree.UID)
	initializing := false
	failedMessage := ""
	generatedWorkspace := worktree.Labels[worktreeownership.GeneratedForLabel]
	for index := range pods.Items {
		pod := &pods.Items[index]
		if !podUsesPersistentVolumeClaim(pod, claimName) {
			continue
		}
		if generatedWorkspace != "" && pod.Spec.OS != nil && pod.Spec.OS.Name == corev1.Windows && pod.Labels["workspaces.rc.ayaka.io/workspace"] == generatedWorkspace {
			// Windows runs lifecycle initialization before starting rc-kube in
			// its main container, not in Kubernetes init containers. Its health
			// probe can succeed only after every initializer has completed.
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue && pod.Status.Phase == corev1.PodRunning {
					return r.setWorktreeStatus(ctx, worktree, metav1.ConditionTrue, "WorktreeReady", "Windows Workspace initialized the isolated Git checkout", claimName, sourceClaimName, path)
				}
			}
			initializing = true
		}
		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name != containerName {
				continue
			}
			terminated := status.State.Terminated
			if terminated == nil && status.State.Waiting != nil {
				terminated = status.LastTerminationState.Terminated
			}
			if terminated != nil {
				if terminated.ExitCode == 0 {
					return r.setWorktreeStatus(ctx, worktree, metav1.ConditionTrue, "WorktreeReady", "Workspace runtime initialized the isolated Git checkout", claimName, sourceClaimName, path)
				}
				failedMessage = fmt.Sprintf("Workspace bootstrap init container exited with code %d", terminated.ExitCode)
				continue
			}
			initializing = true
		}
	}
	if failedMessage != "" {
		return r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "BootstrapFailed", failedMessage, claimName, sourceClaimName, path)
	}
	if initializing {
		return r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "Initializing", "Workspace bootstrap init container is running", claimName, sourceClaimName, path)
	}

	return r.setWorktreeStatus(ctx, worktree, metav1.ConditionFalse, "WaitingForWorkspace", "Child volume is waiting for a Workspace runtime to initialize its Git branch", claimName, sourceClaimName, path)
}

func worktreeVolumeClaim(
	worktree *repositoriesv1alpha1.Worktree,
	claimName, sourceClaimName string,
	plan worktreestorage.Plan,
) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       claimName,
			Namespace:  worktree.Namespace,
			Finalizers: []string{worktreeownership.VolumeProtectionFinalizer},
			Labels: map[string]string{
				worktreeManagedByLabel:  worktreeManagedByValue,
				worktreeRepositoryLabel: worktree.Spec.RepositoryRef.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      slices.Clone(plan.AccessModes),
			StorageClassName: &plan.StorageClassName,
			// A Git checkout requires a mounted filesystem.
			VolumeMode: new(corev1.PersistentVolumeFilesystem),
			DataSource: &corev1.TypedLocalObjectReference{
				Kind: "PersistentVolumeClaim",
				Name: sourceClaimName,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: plan.Size.DeepCopy()},
			},
		},
	}
}

// worktreeClaimMatches validates immutable explicit storage intent. Clone source
// validation moved to worktreestorage.CloneSourceName so recovery no longer
// compares committed identity to mutable Repository or Worktree status.
// Inherited defaults belong to the existing PVC, including legacy RWX defaults.
func worktreeClaimMatches(claim *corev1.PersistentVolumeClaim, override *repositoriesv1alpha1.WorktreeStorageSpec) bool {
	if claim.Spec.VolumeMode != nil && *claim.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return false
	}
	if override == nil {
		return true
	}
	if override.StorageClassName != "" && (claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != override.StorageClassName) {
		return false
	}
	if len(override.AccessModes) > 0 && !slices.Equal(claim.Spec.AccessModes, override.AccessModes) {
		return false
	}
	currentSize := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	return override.Size == nil || currentSize.Cmp(*override.Size) >= 0
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
	if worktree.Status.WorktreePath != "" {
		return worktree.Status.WorktreePath
	}

	return workerMountPath
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
	gitArgs := worktreeCheckoutArgs(worktree)
	volumeRootMountPath := worktreebootstrap.VolumeRootMountPath(worktree.Name)
	noCheckout := fmt.Sprintf("%t", worktree.Spec.NoCheckout)

	backoffLimit := int32(0)
	ttlSecondsAfterFinished := worktreeBootstrapJobTTL
	allowPrivilegeEscalation := false
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      worktreeBootstrapJobName(worktree),
			Namespace: worktree.Namespace,
			Annotations: map[string]string{
				worktreePathAnnotation: workerMountPath,
			},
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
						Args:       append([]string{"-ceu", worktreeBootstrapScript, "worktree-bootstrap", noCheckout}, gitArgs...),
						WorkingDir: volumeRootMountPath,
						Env:        []corev1.EnvVar{{Name: "HOME", Value: "/tmp"}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPrivilegeEscalation,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCapabilitiesDrop}},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: workerVolumeName, MountPath: volumeRootMountPath}},
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

func worktreeCheckoutArgs(worktree *repositoriesv1alpha1.Worktree) []string {
	gitArgs := []string{gitCheckoutSubcommand}
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
		gitArgs = append(gitArgs, "--orphan", worktree.Name)
	}
	if worktree.Spec.Branch == "" && worktree.Spec.ResetBranch == "" && !worktree.Spec.Detach && !worktree.Spec.Orphan && worktree.Spec.Ref == "" {
		gitArgs = append(gitArgs, "-b", worktree.Name)
	}
	if worktree.Spec.Ref != "" {
		gitArgs = append(gitArgs, worktree.Spec.Ref)
	}

	return gitArgs
}

func (r *WorktreeReconciler) setWorktreeVolumeReady(ctx context.Context, worktree *repositoriesv1alpha1.Worktree, claimName, sourceClaimName, path string) error {
	key := client.ObjectKeyFromObject(worktree)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Worktree)
		if err := r.Get(ctx, key, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		before := current.DeepCopy()
		current.Status.ObservedGeneration = current.Generation
		current.Status.SourceVolumeClaimName = sourceClaimName
		current.Status.VolumeClaimName = claimName
		current.Status.WorktreePath = path
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: repositoriesv1alpha1.WorktreeConditionVolumeReady, Status: metav1.ConditionTrue,
			ObservedGeneration: current.Generation, Reason: "VolumeReady", Message: "Child volume is bound and ready to mount",
		})
		if current.Status.ObservedGeneration == before.Status.ObservedGeneration &&
			current.Status.SourceVolumeClaimName == before.Status.SourceVolumeClaimName &&
			current.Status.VolumeClaimName == before.Status.VolumeClaimName &&
			current.Status.WorktreePath == before.Status.WorktreePath &&
			slices.Equal(current.Status.Conditions, before.Status.Conditions) {
			return nil
		}
		if err := r.Status().Patch(ctx, current, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("patch Worktree volume-ready status: %w", err)
		}

		return nil
	})
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
		if claimName != "" {
			current.Status.VolumeClaimName = claimName
		}
		current.Status.WorktreePath = path
		// Storage failures also withdraw VolumeReady: deferred Workspaces mount on
		// VolumeReady alone and must not mount a lost or foreign claim.
		if reason == repositoriesv1alpha1.WorktreeReasonVolumeClaimLost || reason == repositoriesv1alpha1.WorktreeReasonVolumeClaimConflict {
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type: repositoriesv1alpha1.WorktreeConditionVolumeReady, Status: metav1.ConditionFalse,
				ObservedGeneration: current.Generation, Reason: reason, Message: message,
			})
		}
		if claimName == "" || worktreebootstrap.Deferred(current) {
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
			slices.Equal(current.Status.Conditions, before.Status.Conditions) {
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
	r.APIReader = mgr.GetAPIReader()
	if r.RunnerImage == "" {
		return fmt.Errorf("worktree runner image must not be empty")
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &repositoriesv1alpha1.Worktree{}, worktreeRepositoryIndex, worktreeRepositoryIndexValues); err != nil {
		return fmt.Errorf("index Worktrees by Repository: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&repositoriesv1alpha1.Worktree{}).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.worktreesForClaim)).
		Owns(&batchv1.Job{}).
		Watches(&workspacesv1alpha1.Workspace{}, handler.EnqueueRequestsFromMapFunc(r.worktreesForWorkspace)).
		Watches(&repositoriesv1alpha1.WorktreeExec{}, handler.EnqueueRequestsFromMapFunc(worktreeForExec)).
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
					continue
				}
				if podUsesPersistentVolumeClaim(pod, claimName) {
					requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(worktree)})
				}
			}

			return requests
		})).
		Watches(&repositoriesv1alpha1.Repository{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []ctrl.Request {
			return r.worktreesForRepository(ctx, client.ObjectKeyFromObject(object))
		})).
		Named("repositories-worktree").
		Complete(r)
}

const worktreeBootstrapScript = `
git config --global --add safe.directory "$PWD"
no_checkout="$1"
shift
git -C "$PWD" -c checkout.workers=8 -c checkout.thresholdForParallelism=100 "$@"
if [ "$no_checkout" = "true" ]; then
  git -C "$PWD" read-tree --empty
  git -C "$PWD" clean -ffdx
fi
git -C "$PWD" rev-parse --verify HEAD || git -C "$PWD" symbolic-ref --quiet HEAD
`
