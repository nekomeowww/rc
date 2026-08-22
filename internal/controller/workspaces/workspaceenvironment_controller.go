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

package workspaces

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const environmentManagedByLabel = "workspaces.rc.ayaka.io/environment"

// WorkspaceEnvironmentReconciler reconciles committed Environment home state.
type WorkspaceEnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaceenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaceenvironments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaceenvironments/finalizers,verbs=update
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=agentprocesses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete

func (r *WorkspaceEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	environment := new(workspacesv1alpha1.WorkspaceEnvironment)
	if err := r.Get(ctx, req.NamespacedName, environment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	commitHandled, err := r.reconcileEnvironmentCommit(ctx, environment)
	if err != nil {
		return ctrl.Result{}, err
	}
	if commitHandled {
		return ctrl.Result{}, nil
	}
	result, handled, err := r.reconcileEditorLifecycle(ctx, environment)
	if err != nil || handled {
		return result, err
	}

	revision := environment.Status.CurrentRevision
	if revision == 0 {
		revision = 1
	}
	claimName := environment.Status.CurrentVolumeClaimName
	if claimName == "" {
		claimName = environmentCurrentClaimName(environment.Name, revision)
	}
	committedImage := environment.Status.CurrentImage
	if committedImage == "" {
		committedImage = environment.Spec.Image
	}

	claim := new(corev1.PersistentVolumeClaim)
	claimKey := types.NamespacedName{Name: claimName, Namespace: environment.Namespace}
	err = r.Get(ctx, claimKey, claim)
	if errors.IsNotFound(err) {
		claim = environmentVolumeClaim(environment, claimName, "")
		if err := controllerutil.SetControllerReference(environment, claim, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set WorkspaceEnvironment owner on PersistentVolumeClaim: %w", err)
		}
		if err := r.Create(ctx, claim); err != nil {
			return ctrl.Result{}, fmt.Errorf("create WorkspaceEnvironment current PersistentVolumeClaim: %w", err)
		}
		log.Info("Created WorkspaceEnvironment current PersistentVolumeClaim", "name", claim.Name)

		return ctrl.Result{}, r.setEnvironmentStatus(ctx, req.NamespacedName, revision, committedImage, claim.Name, metav1.ConditionFalse, "Provisioning", "Current volume is provisioning")
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get WorkspaceEnvironment current PersistentVolumeClaim: %w", err)
	}
	if !metav1.IsControlledBy(claim, environment) {
		return ctrl.Result{}, r.setEnvironmentStatus(ctx, req.NamespacedName, revision, committedImage, "", metav1.ConditionFalse, "VolumeClaimConflict", "Current PersistentVolumeClaim is not owned by this WorkspaceEnvironment")
	}
	if !environmentStorageMatches(claim, environment.Spec.Storage) {
		return ctrl.Result{}, r.setEnvironmentStatus(ctx, req.NamespacedName, revision, committedImage, claim.Name, metav1.ConditionFalse, "VolumeClaimSpecChanged", "Changing committed Environment storage is not supported")
	}
	if claim.Status.Phase != corev1.ClaimBound {
		if failureReason, failureMessage, failureErr := (&WorkspaceReconciler{Client: r.Client}).persistentVolumeClaimFailure(ctx, claim); failureErr != nil {
			return ctrl.Result{}, failureErr
		} else if failureReason != "" {
			return ctrl.Result{}, r.setEnvironmentStatus(ctx, req.NamespacedName, revision, committedImage, claim.Name, metav1.ConditionFalse, failureReason, failureMessage)
		}
		return ctrl.Result{}, r.setEnvironmentStatus(ctx, req.NamespacedName, revision, committedImage, claim.Name, metav1.ConditionFalse, "Provisioning", "Current volume is provisioning")
	}

	return ctrl.Result{}, r.setEnvironmentStatus(ctx, req.NamespacedName, revision, committedImage, claim.Name, metav1.ConditionTrue, "EnvironmentReady", "Current Environment revision is ready")
}

func (r *WorkspaceEnvironmentReconciler) reconcileEditorLifecycle(ctx context.Context, environment *workspacesv1alpha1.WorkspaceEnvironment) (ctrl.Result, bool, error) {
	if environment.Status.EditorPodName == "" {
		return ctrl.Result{}, false, nil
	}
	editor := new(corev1.Pod)
	key := types.NamespacedName{Name: environment.Status.EditorPodName, Namespace: environment.Namespace}
	if err := r.Get(ctx, key, editor); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, true, fmt.Errorf("get Environment editor Pod: %w", err)
		}
		current := new(workspacesv1alpha1.WorkspaceEnvironment)
		if err := r.Get(ctx, client.ObjectKeyFromObject(environment), current); err != nil {
			return ctrl.Result{}, true, err
		}
		current.Status.EditorPodName = ""
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: workspacesv1alpha1.WorkspaceEnvironmentConditionDraftReady, Status: metav1.ConditionFalse,
			ObservedGeneration: current.Generation, Reason: "EditorStopped", Message: "Environment draft is retained while the editor is stopped",
		})
		if err := r.Status().Update(ctx, current); err != nil {
			return ctrl.Result{}, true, fmt.Errorf("clear stopped Environment editor status: %w", err)
		}
		return ctrl.Result{}, true, nil
	}
	if environment.Spec.EditorIdleTimeout == nil || environment.Spec.EditorIdleTimeout.Duration == 0 {
		return ctrl.Result{}, false, nil
	}
	processes := new(workspacesv1alpha1.AgentProcessList)
	if err := r.List(ctx, processes, client.InNamespace(environment.Namespace)); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("list Environment editor processes: %w", err)
	}
	var lastCompletion *metav1.Time
	for index := range processes.Items {
		process := &processes.Items[index]
		if process.Spec.TargetRef.Kind != workspacesv1alpha1.AgentProcessTargetWorkspaceEnvironment || process.Spec.TargetRef.Name != environment.Name {
			continue
		}
		if !agentProcessTerminal(process.Status.Phase) {
			return ctrl.Result{}, false, nil
		}
		if process.Status.CompletedAt != nil && (lastCompletion == nil || process.Status.CompletedAt.After(lastCompletion.Time)) {
			lastCompletion = process.Status.CompletedAt
		}
	}
	if lastCompletion == nil {
		return ctrl.Result{}, false, nil
	}
	deadline := lastCompletion.Add(environment.Spec.EditorIdleTimeout.Duration)
	if time.Now().Before(deadline) {
		return ctrl.Result{RequeueAfter: time.Until(deadline)}, true, nil
	}
	if err := r.Delete(ctx, editor); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("delete idle Environment editor Pod: %w", err)
	}

	return ctrl.Result{}, true, nil
}

func (r *WorkspaceEnvironmentReconciler) reconcileEnvironmentCommit(ctx context.Context, environment *workspacesv1alpha1.WorkspaceEnvironment) (bool, error) {
	if environment.Spec.Commit <= environment.Status.CommittedRequest {
		return false, nil
	}
	if environment.Status.DraftVolumeClaimName == "" {
		return true, r.setEnvironmentDraftCondition(ctx, client.ObjectKeyFromObject(environment), "NoDraft", "Environment has no draft to commit")
	}
	processes := new(workspacesv1alpha1.AgentProcessList)
	if err := r.List(ctx, processes, client.InNamespace(environment.Namespace)); err != nil {
		return true, fmt.Errorf("list Environment draft Agent Processes: %w", err)
	}
	for index := range processes.Items {
		process := &processes.Items[index]
		if process.Spec.TargetRef.Kind == workspacesv1alpha1.AgentProcessTargetWorkspaceEnvironment && process.Spec.TargetRef.Name == environment.Name && !agentProcessTerminal(process.Status.Phase) {
			return true, r.setEnvironmentDraftCondition(ctx, client.ObjectKeyFromObject(environment), "ActiveProcesses", "Environment draft cannot commit while Agent Processes are active")
		}
	}
	if environment.Status.EditorPodName != "" {
		editor := new(corev1.Pod)
		key := types.NamespacedName{Name: environment.Status.EditorPodName, Namespace: environment.Namespace}
		if err := r.Get(ctx, key, editor); err == nil {
			if err := r.Delete(ctx, editor); err != nil {
				return true, fmt.Errorf("delete Environment editor Pod before commit: %w", err)
			}

			return true, r.setEnvironmentDraftCondition(ctx, client.ObjectKeyFromObject(environment), "StoppingEditor", "Environment editor Pod is stopping before commit")
		} else if !errors.IsNotFound(err) {
			return true, fmt.Errorf("get Environment editor Pod before commit: %w", err)
		}
	}
	draft := new(corev1.PersistentVolumeClaim)
	draftKey := types.NamespacedName{Name: environment.Status.DraftVolumeClaimName, Namespace: environment.Namespace}
	if err := r.Get(ctx, draftKey, draft); err != nil {
		if errors.IsNotFound(err) {
			return true, r.setEnvironmentDraftCondition(ctx, client.ObjectKeyFromObject(environment), "DraftMissing", "Environment draft volume does not exist")
		}
		return true, fmt.Errorf("get Environment draft PersistentVolumeClaim: %w", err)
	}
	if draft.Status.Phase != corev1.ClaimBound {
		return true, r.setEnvironmentDraftCondition(ctx, client.ObjectKeyFromObject(environment), "DraftNotReady", "Environment draft volume is not bound")
	}
	oldCurrentName := environment.Status.CurrentVolumeClaimName
	current := new(workspacesv1alpha1.WorkspaceEnvironment)
	if err := r.Get(ctx, client.ObjectKeyFromObject(environment), current); err != nil {
		return true, fmt.Errorf("re-fetch WorkspaceEnvironment before commit: %w", err)
	}
	current.Status.ObservedGeneration = current.Generation
	current.Status.CurrentRevision++
	if current.Status.CurrentRevision == 0 {
		current.Status.CurrentRevision = 1
	}
	current.Status.CurrentImage = current.Spec.Image
	current.Status.CurrentVolumeClaimName = draft.Name
	current.Status.DraftVolumeClaimName = ""
	current.Status.EditorPodName = ""
	current.Status.CommittedRequest = current.Spec.Commit
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.WorkspaceEnvironmentConditionReady, Status: metav1.ConditionTrue,
		ObservedGeneration: current.Generation, Reason: "EnvironmentReady", Message: "Committed Environment revision is ready",
	})
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.WorkspaceEnvironmentConditionDraftReady, Status: metav1.ConditionFalse,
		ObservedGeneration: current.Generation, Reason: "Committed", Message: "Environment draft was promoted",
	})
	if err := r.Status().Update(ctx, current); err != nil {
		return true, fmt.Errorf("promote Environment draft: %w", err)
	}
	if oldCurrentName != "" && oldCurrentName != draft.Name {
		oldCurrent := new(corev1.PersistentVolumeClaim)
		oldKey := types.NamespacedName{Name: oldCurrentName, Namespace: environment.Namespace}
		if err := r.Get(ctx, oldKey, oldCurrent); err == nil {
			if err := r.Delete(ctx, oldCurrent); err != nil {
				return true, fmt.Errorf("delete previous Environment current PersistentVolumeClaim: %w", err)
			}
		} else if !errors.IsNotFound(err) {
			return true, fmt.Errorf("get previous Environment current PersistentVolumeClaim: %w", err)
		}
	}

	return true, nil
}

func (r *WorkspaceEnvironmentReconciler) setEnvironmentDraftCondition(ctx context.Context, key types.NamespacedName, reason string, message string) error {
	current := new(workspacesv1alpha1.WorkspaceEnvironment)
	if err := r.Get(ctx, key, current); err != nil {
		return fmt.Errorf("re-fetch WorkspaceEnvironment before draft condition update: %w", err)
	}
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.WorkspaceEnvironmentConditionDraftReady, Status: metav1.ConditionFalse,
		ObservedGeneration: current.Generation, Reason: reason, Message: message,
	})
	if err := r.Status().Update(ctx, current); err != nil {
		return fmt.Errorf("update WorkspaceEnvironment draft condition: %w", err)
	}

	return nil
}

func environmentCurrentClaimName(name string, revision int64) string {
	return fmt.Sprintf("%s-current-%d", name, revision)
}

func environmentVolumeClaim(environment *workspacesv1alpha1.WorkspaceEnvironment, name string, source string) *corev1.PersistentVolumeClaim {
	storage := environment.Spec.Storage
	volumeMode := storage.VolumeModeOrDefault()
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: environment.Namespace,
			Labels: map[string]string{
				environmentManagedByLabel: environment.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      storage.AccessModesOrDefault(),
			StorageClassName: storageClassNamePointer(storage.StorageClassName),
			VolumeMode:       &volumeMode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storage.Size},
			},
		},
	}
	if source != "" {
		claim.Spec.DataSource = &corev1.TypedLocalObjectReference{Kind: persistentVolumeClaimKind, Name: source}
	}

	return claim
}

func environmentClaimMatches(claim *corev1.PersistentVolumeClaim, storage workspacesv1alpha1.PersistentStorageSpec, source string) bool {
	if !environmentStorageMatches(claim, storage) {
		return false
	}
	if source == "" {
		return claim.Spec.DataSource == nil
	}

	return claim.Spec.DataSource != nil && claim.Spec.DataSource.Kind == "PersistentVolumeClaim" && claim.Spec.DataSource.Name == source
}

func environmentStorageMatches(claim *corev1.PersistentVolumeClaim, storage workspacesv1alpha1.PersistentStorageSpec) bool {
	if storage.StorageClassName != "" && (claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != storage.StorageClassName) {
		return false
	}
	if claim.Spec.VolumeMode == nil || *claim.Spec.VolumeMode != storage.VolumeModeOrDefault() {
		return false
	}
	if !equalAccessModes(claim.Spec.AccessModes, storage.AccessModesOrDefault()) {
		return false
	}
	quantity, ok := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok || quantity.Cmp(storage.Size) != 0 {
		return false
	}
	return true
}

func equalAccessModes(left []corev1.PersistentVolumeAccessMode, right []corev1.PersistentVolumeAccessMode) bool {
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

func (r *WorkspaceEnvironmentReconciler) setEnvironmentStatus(
	ctx context.Context,
	key types.NamespacedName,
	revision int64,
	image string,
	claimName string,
	conditionStatus metav1.ConditionStatus,
	reason string,
	message string,
) error {
	current := new(workspacesv1alpha1.WorkspaceEnvironment)
	if err := r.Get(ctx, key, current); err != nil {
		return fmt.Errorf("re-fetch WorkspaceEnvironment before status update: %w", err)
	}
	current.Status.ObservedGeneration = current.Generation
	current.Status.CurrentRevision = revision
	current.Status.CurrentImage = image
	current.Status.CurrentVolumeClaimName = claimName
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type:               workspacesv1alpha1.WorkspaceEnvironmentConditionReady,
		Status:             conditionStatus,
		ObservedGeneration: current.Generation,
		Reason:             reason,
		Message:            message,
	})
	if err := r.Status().Update(ctx, current); err != nil {
		return fmt.Errorf("update WorkspaceEnvironment status: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.WorkspaceEnvironment{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.Pod{}).
		Watches(&workspacesv1alpha1.AgentProcess{}, handler.EnqueueRequestsFromMapFunc(environmentForProcess)).
		Named("workspaces-workspaceenvironment").
		Complete(r)
}

func environmentForProcess(_ context.Context, object client.Object) []reconcile.Request {
	process, ok := object.(*workspacesv1alpha1.AgentProcess)
	if !ok || process.Spec.TargetRef.Kind != workspacesv1alpha1.AgentProcessTargetWorkspaceEnvironment {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: process.Spec.TargetRef.Name, Namespace: process.Namespace}}}
}
