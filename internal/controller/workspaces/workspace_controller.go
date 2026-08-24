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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/lifecycle"
	"github.com/nekomeowww/rc/internal/runtimepolicy"
	"github.com/nekomeowww/rc/internal/worktreebootstrap"
)

const (
	defaultWorkspaceServiceAccount   = "rc-workspace"
	workspaceTopologyAnnotation      = "workspaces.rc.ayaka.io/topology"
	workspaceManagedByLabel          = "workspaces.rc.ayaka.io/workspace"
	workspaceHomeVolumeName          = "home"
	workspaceRuntimeVolumeName       = "rc-runtime"
	workspaceHomeMountPath           = "/home/agent"
	workspaceRootMountPath           = "/workspace"
	workspaceDependencyRequeue       = 2 * time.Second
	workspaceFinalizer               = "workspaces.rc.ayaka.io/terminate-processes"
	workspaceImageAnnotation         = "workspaces.rc.ayaka.io/runtime-image"
	workspaceRevisionAnnotation      = "workspaces.rc.ayaka.io/source-revision"
	workspaceWriteClaimsAnnotation   = "workspaces.rc.ayaka.io/write-claims"
	workspaceRuntimePolicyAnnotation = "workspaces.rc.ayaka.io/runtime-policy"
	workspaceRuntimePolicyVersion    = "restricted-v1"
	repositoryRootMountPath          = "/repository"
	workspaceWriteHolderLabel        = "workspaces.rc.ayaka.io/write-holder"
	workspaceWriteClaimPrefix        = "rc-worktree-"
	runtimeContainerName             = "rc-kube"
	runtimeServeArgument             = "serve"
	allLinuxCapabilities             = corev1.Capability("ALL")
	persistentVolumeClaimKind        = "PersistentVolumeClaim"
	reasonTargetNotReady             = "TargetNotReady"
	agentTypeCodex                   = "codex"
	defaultCredentialName            = "default"
	verbCreate                       = "create"
	verbDelete                       = "delete"
	verbGet                          = "get"
	verbList                         = "list"
	verbPatch                        = "patch"
	verbUpdate                       = "update"
	verbWatch                        = "watch"
)

type resolvedWorkspace struct {
	image            string
	revision         int64
	storage          workspacesv1alpha1.PersistentStorageSpec
	sourceClaimName  string
	volumeMounts     []corev1.VolumeMount
	volumes          []corev1.Volume
	outdated         bool
	serviceAccount   string
	automountSAToken bool
	writeClaims      []workspaceWriteClaim
	initializers     []workspaceInitializer
	beforeStop       []lifecycle.Action
}

type workspaceInitializer struct {
	name   string
	image  string
	action lifecycle.Action
}

type workspaceWriteClaim struct {
	leaseName string
	worktree  string
}

// WorkspaceReconciler reconciles persistent Workspace storage and runtime Pods.
type WorkspaceReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	RunnerImage string
}

// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaceenvironments;agentprocesses,verbs=get;list;watch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositories;worktrees,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims;pods;serviceaccounts;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

//nolint:gocyclo // Reconcile is an explicit lifecycle state machine with guarded transitions.
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	workspace := new(workspacesv1alpha1.Workspace)
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return r.finalizeWorkspace(ctx, workspace)
	}
	if !controllerutil.ContainsFinalizer(workspace, workspaceFinalizer) {
		controllerutil.AddFinalizer(workspace, workspaceFinalizer)
		if err := r.Update(ctx, workspace); err != nil {
			return ctrl.Result{}, fmt.Errorf("add Workspace finalizer: %w", err)
		}
	}

	resolved, reason, message, err := r.resolveWorkspaceBase(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		result := ctrl.Result{}
		if strings.HasSuffix(reason, "NotReady") || strings.HasSuffix(reason, "NotFound") || reason == "WorktreeInUse" {
			result.RequeueAfter = workspaceDependencyRequeue
		}
		return result, r.setWorkspaceStatus(ctx, req.NamespacedName, nil, metav1.ConditionFalse, reason, message)
	}

	home := new(corev1.PersistentVolumeClaim)
	err = r.Get(ctx, req.NamespacedName, home)
	if errors.IsNotFound(err) {
		home = workspaceHomeVolumeClaim(workspace, resolved)
		if err := controllerutil.SetControllerReference(workspace, home, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set Workspace owner on home PersistentVolumeClaim: %w", err)
		}
		if err := r.Create(ctx, home); err != nil {
			return ctrl.Result{}, fmt.Errorf("create Workspace home PersistentVolumeClaim: %w", err)
		}
		log.Info("Created Workspace home PersistentVolumeClaim", "name", home.Name)

		return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "Provisioning", "Workspace home volume is provisioning")
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Workspace home PersistentVolumeClaim: %w", err)
	}
	if !metav1.IsControlledBy(home, workspace) {
		return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "VolumeClaimConflict", "Workspace home PersistentVolumeClaim is not owned by this Workspace")
	}
	if workspace.Status.RuntimeImage == "" && home.Annotations[workspaceImageAnnotation] != "" {
		resolved.image = home.Annotations[workspaceImageAnnotation]
		if revision, parseErr := strconv.ParseInt(home.Annotations[workspaceRevisionAnnotation], 10, 64); parseErr == nil {
			resolved.revision = revision
		}
		if workspace.Spec.EnvironmentRef != nil {
			environment := new(workspacesv1alpha1.WorkspaceEnvironment)
			key := types.NamespacedName{Name: workspace.Spec.EnvironmentRef.Name, Namespace: workspace.Namespace}
			if err := r.Get(ctx, key, environment); err != nil {
				return ctrl.Result{}, fmt.Errorf("re-read WorkspaceEnvironment for captured home metadata: %w", err)
			}
			resolved.outdated = resolved.revision != environment.Status.CurrentRevision || resolved.image != environment.Status.CurrentImage
		}
	}
	if home.Status.Phase != corev1.ClaimBound {
		if failureReason, failureMessage, failureErr := r.persistentVolumeClaimFailure(ctx, home); failureErr != nil {
			return ctrl.Result{}, failureErr
		} else if failureReason != "" {
			return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, failureReason, failureMessage)
		}
		return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "Provisioning", "Workspace home volume is provisioning")
	}
	resolved, reason, message, err = r.resolveWorkspaceDependencies(ctx, workspace, resolved)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		result := ctrl.Result{}
		if strings.HasSuffix(reason, "NotReady") || strings.HasSuffix(reason, "NotFound") || reason == "WorktreeInUse" {
			result.RequeueAfter = workspaceDependencyRequeue
		}
		return result, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, reason, message)
	}

	active, hasProcesses, lastCompletion, err := r.processState(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	desiredState := workspace.Spec.DesiredState
	if desiredState == "" {
		desiredState = workspacesv1alpha1.WorkspaceDesiredStateRunning
	}
	readyCondition := meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
	if workspace.Spec.Generated && hasProcesses && !active && lastCompletion != nil && desiredState == workspacesv1alpha1.WorkspaceDesiredStateRunning && readyCondition != nil && readyCondition.Status == metav1.ConditionTrue && (workspace.Status.LastAutoSuspendTime == nil || lastCompletion.After(workspace.Status.LastAutoSuspendTime.Time)) {
		workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
		if err := r.Update(ctx, workspace); err != nil {
			return ctrl.Result{}, fmt.Errorf("auto-suspend generated Workspace: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if !workspace.Spec.Generated && workspace.Spec.IdleTimeout != nil && workspace.Spec.IdleTimeout.Duration > 0 && hasProcesses && !active && desiredState == workspacesv1alpha1.WorkspaceDesiredStateRunning && readyCondition != nil && readyCondition.Status == metav1.ConditionTrue {
		lastActivity := readyCondition.LastTransitionTime.Time
		if lastCompletion != nil && lastCompletion.After(lastActivity) {
			lastActivity = lastCompletion.Time
		}
		deadline := lastActivity.Add(workspace.Spec.IdleTimeout.Duration)
		if time.Now().Before(deadline) {
			return ctrl.Result{RequeueAfter: time.Until(deadline)}, nil
		}
		workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
		if err := r.Update(ctx, workspace); err != nil {
			return ctrl.Result{}, fmt.Errorf("suspend idle Workspace: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if desiredState == workspacesv1alpha1.WorkspaceDesiredStateSuspended {
		if active {
			return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "ActiveProcesses", "Workspace cannot suspend while Agent Processes are active")
		}
		if workspace.Spec.Generated && lastCompletion != nil && (workspace.Status.LastAutoSuspendTime == nil || lastCompletion.After(workspace.Status.LastAutoSuspendTime.Time)) {
			workspace.Status.LastAutoSuspendTime = lastCompletion.DeepCopy()
			if err := r.Status().Update(ctx, workspace); err != nil {
				return ctrl.Result{}, fmt.Errorf("record generated Workspace auto-suspension: %w", err)
			}
			return ctrl.Result{Requeue: true}, nil
		}
		pod := new(corev1.Pod)
		if err := r.Get(ctx, req.NamespacedName, pod); err == nil {
			if !metav1.IsControlledBy(pod, workspace) {
				return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "RuntimePodConflict", "A Pod with the Workspace runtime name exists but is not owned by this Workspace")
			}
			if err := r.Delete(ctx, pod); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete suspended Workspace runtime Pod: %w", err)
			}
			log.Info("Deleted Workspace runtime Pod", "name", pod.Name)
			return ctrl.Result{RequeueAfter: workspaceDependencyRequeue}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "Stopping", "Workspace runtime Pod is stopping")
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get suspended Workspace runtime Pod: %w", err)
		}
		if err := r.releaseWriteClaims(ctx, workspace, nil); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "Suspended", "Workspace runtime is suspended")
	}

	if resolved.automountSAToken && resolved.serviceAccount == defaultWorkspaceServiceAccount {
		if err := r.ensureWorkspaceAccess(ctx, workspace.Namespace); err != nil {
			return ctrl.Result{}, err
		}
	}

	pod := new(corev1.Pod)
	err = r.Get(ctx, req.NamespacedName, pod)
	if errors.IsNotFound(err) {
		if err := r.releaseWriteClaims(ctx, workspace, resolved.writeClaims); err != nil {
			return ctrl.Result{}, err
		}
		acquired, claimMessage, err := r.acquireWriteClaims(ctx, workspace, resolved.writeClaims)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !acquired {
			return ctrl.Result{RequeueAfter: workspaceDependencyRequeue}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "WorktreeInUse", claimMessage)
		}
		pod, err = workspaceRuntimePod(workspace, resolved)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := controllerutil.SetControllerReference(workspace, pod, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set Workspace owner on runtime Pod: %w", err)
		}
		if err := r.Create(ctx, pod); err != nil {
			return ctrl.Result{}, fmt.Errorf("create Workspace runtime Pod: %w", err)
		}
		log.Info("Created Workspace runtime Pod", "name", pod.Name)

		return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "Starting", "Workspace runtime Pod is starting")
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Workspace runtime Pod: %w", err)
	}
	if !metav1.IsControlledBy(pod, workspace) {
		return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "RuntimePodConflict", "A Pod with the Workspace runtime name exists but is not owned by this Workspace")
	}
	expectedTopology, err := workspaceTopologyHash(workspace, resolved)
	if err != nil {
		return ctrl.Result{}, err
	}
	currentClaims, err := workspacePodWriteClaims(pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(currentClaims) == 0 && pod.Annotations[workspaceTopologyAnnotation] == expectedTopology {
		currentClaims = resolved.writeClaims
	}
	acquired, claimMessage, err := r.acquireWriteClaims(ctx, workspace, currentClaims)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !acquired {
		if pod.DeletionTimestamp == nil {
			if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("stop Workspace runtime after losing Worktree write Lease: %w", err)
			}
		}
		return ctrl.Result{RequeueAfter: workspaceDependencyRequeue}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "WorktreeInUse", claimMessage)
	}
	if pod.Annotations[workspaceTopologyAnnotation] != expectedTopology {
		if active {
			return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "TopologyChangeBlocked", "Workspace topology changed while Agent Processes are active")
		}
		if err := r.Delete(ctx, pod); err != nil {
			return ctrl.Result{}, fmt.Errorf("replace Workspace runtime Pod: %w", err)
		}

		return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "Replacing", "Workspace runtime Pod topology changed")
	}
	if failureReason, failureMessage := workspaceInitializationFailure(pod); failureReason != "" {
		return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, failureReason, failureMessage)
	}
	if !podReady(pod) {
		return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionFalse, "Starting", "Workspace runtime Pod is starting")
	}

	return ctrl.Result{}, r.setWorkspaceStatus(ctx, req.NamespacedName, resolved, metav1.ConditionTrue, "WorkspaceReady", "Workspace runtime is ready")
}

func workspaceInitializationFailure(pod *corev1.Pod) (string, string) {
	for _, status := range pod.Status.InitContainerStatuses {
		terminated := status.State.Terminated
		if terminated == nil && status.State.Waiting != nil {
			terminated = status.LastTerminationState.Terminated
		}
		if terminated != nil && terminated.ExitCode != 0 {
			return "InitializationFailed", fmt.Sprintf("Workspace initializer %s exited with code %d", status.Name, terminated.ExitCode)
		}
	}

	return "", ""
}

func (r *WorkspaceReconciler) finalizeWorkspace(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(workspace, workspaceFinalizer) {
		return ctrl.Result{}, nil
	}
	processes := new(workspacesv1alpha1.AgentProcessList)
	if err := r.List(ctx, processes, client.InNamespace(workspace.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list Agent Processes while finalizing Workspace: %w", err)
	}
	waiting := false
	for index := range processes.Items {
		process := &processes.Items[index]
		if process.Spec.TargetRef.Kind != workspacesv1alpha1.AgentProcessTargetWorkspace || process.Spec.TargetRef.Name != workspace.Name {
			continue
		}
		waiting = true
		if agentProcessTerminal(process.Status.Phase) {
			if err := r.Delete(ctx, process); err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete terminal Agent Process while finalizing Workspace: %w", err)
			}
		} else if process.Spec.DesiredState != workspacesv1alpha1.AgentProcessDesiredStateStopped {
			process.Spec.DesiredState = workspacesv1alpha1.AgentProcessDesiredStateStopped
			if err := r.Update(ctx, process); err != nil {
				return ctrl.Result{}, fmt.Errorf("stop Agent Process while finalizing Workspace: %w", err)
			}
		}
	}
	if waiting {
		return ctrl.Result{RequeueAfter: workspaceDependencyRequeue}, nil
	}
	pod := new(corev1.Pod)
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), pod); err == nil {
		if metav1.IsControlledBy(pod, workspace) {
			if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete runtime Pod while finalizing Workspace: %w", err)
			}
			return ctrl.Result{RequeueAfter: workspaceDependencyRequeue}, nil
		}
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get runtime Pod while finalizing Workspace: %w", err)
	}
	if err := r.releaseWriteClaims(ctx, workspace, nil); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(workspace, workspaceFinalizer)
	if err := r.Update(ctx, workspace); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove Workspace finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *WorkspaceReconciler) acquireWriteClaims(ctx context.Context, workspace *workspacesv1alpha1.Workspace, claims []workspaceWriteClaim) (bool, string, error) {
	ordered := append([]workspaceWriteClaim(nil), claims...)
	slices.SortFunc(ordered, func(left, right workspaceWriteClaim) int { return strings.Compare(left.leaseName, right.leaseName) })
	created := make([]*coordinationv1.Lease, 0, len(ordered))
	for _, claim := range ordered {
		holder := string(workspace.UID)
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: claim.leaseName, Namespace: workspace.Namespace, Labels: map[string]string{workspaceWriteHolderLabel: workspace.Name}},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
		}
		if err := controllerutil.SetControllerReference(workspace, lease, r.Scheme); err != nil {
			return false, "", fmt.Errorf("set Workspace owner on Worktree write Lease: %w", err)
		}
		if err := r.Create(ctx, lease); err == nil {
			created = append(created, lease)
			continue
		} else if !errors.IsAlreadyExists(err) {
			return false, "", fmt.Errorf("create Worktree write Lease: %w", err)
		}
		current := new(coordinationv1.Lease)
		if err := r.Get(ctx, types.NamespacedName{Name: claim.leaseName, Namespace: workspace.Namespace}, current); err != nil {
			return false, "", fmt.Errorf("get Worktree write Lease: %w", err)
		}
		if current.Spec.HolderIdentity != nil && *current.Spec.HolderIdentity == string(workspace.UID) {
			continue
		}
		for _, acquired := range created {
			_ = r.Delete(ctx, acquired)
		}
		worktree := claim.worktree
		if worktree == "" {
			worktree = claim.leaseName
		}
		return false, fmt.Sprintf("Worktree %s is mounted read-write by another running Workspace", worktree), nil
	}

	return true, "", nil
}

func (r *WorkspaceReconciler) releaseWriteClaims(ctx context.Context, workspace *workspacesv1alpha1.Workspace, keep []workspaceWriteClaim) error {
	kept := make(map[string]struct{}, len(keep))
	for _, claim := range keep {
		kept[claim.leaseName] = struct{}{}
	}
	leases := new(coordinationv1.LeaseList)
	if err := r.List(ctx, leases, client.InNamespace(workspace.Namespace), client.MatchingLabels{workspaceWriteHolderLabel: workspace.Name}); err != nil {
		return fmt.Errorf("list Workspace Worktree write Leases: %w", err)
	}
	for index := range leases.Items {
		lease := &leases.Items[index]
		if _, exists := kept[lease.Name]; exists {
			continue
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(workspace.UID) {
			continue
		}
		if err := r.Delete(ctx, lease); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("release Worktree write Lease: %w", err)
		}
	}

	return nil
}

func (r *WorkspaceReconciler) resolveWorkspaceBase(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (*resolvedWorkspace, string, string, error) {
	resolved := &resolvedWorkspace{image: workspace.Status.RuntimeImage}
	if workspace.Spec.EnvironmentRef != nil {
		environment := new(workspacesv1alpha1.WorkspaceEnvironment)
		key := types.NamespacedName{Name: workspace.Spec.EnvironmentRef.Name, Namespace: workspace.Namespace}
		if err := r.Get(ctx, key, environment); err != nil {
			if errors.IsNotFound(err) {
				return resolved, "EnvironmentNotFound", "Referenced WorkspaceEnvironment does not exist", nil
			}
			return nil, "", "", fmt.Errorf("get WorkspaceEnvironment: %w", err)
		}
		ready := meta.FindStatusCondition(environment.Status.Conditions, workspacesv1alpha1.WorkspaceEnvironmentConditionReady)
		if ready == nil || ready.Status != metav1.ConditionTrue || environment.Status.CurrentVolumeClaimName == "" {
			return resolved, "EnvironmentNotReady", "Referenced WorkspaceEnvironment is not ready", nil
		}
		resolved.storage = environment.Spec.Storage
		resolved.sourceClaimName = environment.Status.CurrentVolumeClaimName
		if resolved.image == "" {
			resolved.image = environment.Status.CurrentImage
			resolved.revision = environment.Status.CurrentRevision
		} else {
			resolved.revision = workspace.Status.SourceEnvironmentRevision
			resolved.outdated = resolved.revision != environment.Status.CurrentRevision || resolved.image != environment.Status.CurrentImage
		}
	} else {
		if workspace.Spec.Storage == nil {
			return resolved, "StorageRequired", "A blank Workspace requires spec.storage", nil
		}
		resolved.storage = *workspace.Spec.Storage
		if resolved.image == "" {
			resolved.image = workspace.Spec.Image
			if resolved.image == "" {
				resolved.image = r.RunnerImage
			}
		}
	}
	if resolved.image == "" {
		return resolved, "ImageRequired", "Workspace runtime image is empty", nil
	}

	return resolved, "", "", nil
}

func (r *WorkspaceReconciler) resolveWorkspaceDependencies(ctx context.Context, workspace *workspacesv1alpha1.Workspace, resolved *resolvedWorkspace) (*resolvedWorkspace, string, string, error) {
	initializerNames := make(map[string]struct{})
	for _, mount := range workspace.Spec.Mounts {
		volume, volumeMount, claim, initializer, reason, message, err := r.resolveWorkspaceMount(ctx, workspace.Namespace, mount)
		if err != nil {
			return nil, "", "", err
		}
		if reason != "" {
			return resolved, reason, message, nil
		}
		resolved.volumes = append(resolved.volumes, volume)
		resolved.volumeMounts = append(resolved.volumeMounts, volumeMount)
		if claim != nil {
			resolved.writeClaims = append(resolved.writeClaims, *claim)
		}
		if initializer != nil {
			if _, exists := initializerNames[initializer.name]; !exists {
				resolved.initializers = append(resolved.initializers, *initializer)
				initializerNames[initializer.name] = struct{}{}
			}
		}
	}
	for index, reference := range workspace.Spec.ConfigMapRefs {
		configMap := new(corev1.ConfigMap)
		if err := r.Get(ctx, types.NamespacedName{Name: reference.Name, Namespace: workspace.Namespace}, configMap); err != nil {
			if errors.IsNotFound(err) {
				return resolved, "ConfigMapNotFound", fmt.Sprintf("Referenced ConfigMap %s does not exist", reference.Name), nil
			}
			return nil, "", "", fmt.Errorf("get Workspace ConfigMap: %w", err)
		}
		volumeName := fmt.Sprintf("config-%d", index)
		resolved.volumes = append(resolved.volumes, corev1.Volume{Name: volumeName, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: reference.Name}}}})
		resolved.volumeMounts = append(resolved.volumeMounts, corev1.VolumeMount{Name: volumeName, MountPath: filepath.Join("/run/rc/configmaps", reference.Name), ReadOnly: true})
	}
	for index, reference := range workspace.Spec.SecretRefs {
		secret := new(corev1.Secret)
		if err := r.Get(ctx, types.NamespacedName{Name: reference.Name, Namespace: workspace.Namespace}, secret); err != nil {
			if errors.IsNotFound(err) {
				return resolved, "SecretNotFound", fmt.Sprintf("Referenced Secret %s does not exist", reference.Name), nil
			}
			return nil, "", "", fmt.Errorf("get Workspace Secret: %w", err)
		}
		volumeName := fmt.Sprintf("secret-%d", index)
		resolved.volumes = append(resolved.volumes, corev1.Volume{Name: volumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: reference.Name}}})
		resolved.volumeMounts = append(resolved.volumeMounts, corev1.VolumeMount{Name: volumeName, MountPath: filepath.Join("/run/rc/secrets", reference.Name), ReadOnly: true})
	}
	if workspace.Spec.Lifecycle != nil {
		for index, action := range workspace.Spec.Lifecycle.Initialize {
			resolvedAction, err := resolveLifecycleAction(action)
			if err != nil {
				return nil, "", "", fmt.Errorf("resolve Workspace initialize action %d: %w", index+1, err)
			}
			resolved.initializers = append(resolved.initializers, workspaceInitializer{
				name: fmt.Sprintf("rc-initialize-%d", index), image: resolved.image, action: resolvedAction,
			})
		}
		for index, action := range workspace.Spec.Lifecycle.BeforeStop {
			resolvedAction, err := resolveLifecycleAction(action)
			if err != nil {
				return nil, "", "", fmt.Errorf("resolve Workspace beforeStop action %d: %w", index+1, err)
			}
			resolved.beforeStop = append(resolved.beforeStop, resolvedAction)
		}
	}

	resolved.automountSAToken = workspace.Spec.AutomountServiceAccountToken == nil || *workspace.Spec.AutomountServiceAccountToken
	if resolved.automountSAToken {
		resolved.serviceAccount = workspace.Spec.ServiceAccountName
		if resolved.serviceAccount == "" {
			resolved.serviceAccount = defaultWorkspaceServiceAccount
		}
	}

	return resolved, "", "", nil
}

func resolveLifecycleAction(action workspacesv1alpha1.WorkspaceLifecycleAction) (lifecycle.Action, error) {
	hasCommand := len(action.Command) > 0
	hasScript := action.Script != ""
	if hasCommand == hasScript {
		return lifecycle.Action{}, fmt.Errorf("exactly one of command or script must be set")
	}
	if hasCommand && action.Command[0] == "" {
		return lifecycle.Action{}, fmt.Errorf("command executable must not be empty")
	}

	return lifecycle.Action{
		Command: append([]string(nil), action.Command...), Script: action.Script, WorkingDirectory: action.WorkingDirectory,
	}, nil
}

func (r *WorkspaceReconciler) resolveWorkspaceMount(ctx context.Context, namespace string, mount workspacesv1alpha1.WorkspaceMount) (corev1.Volume, corev1.VolumeMount, *workspaceWriteClaim, *workspaceInitializer, string, string, error) {
	cleanPath := filepath.Clean(mount.Path)
	if cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") || filepath.IsAbs(mount.Path) {
		return corev1.Volume{}, corev1.VolumeMount{}, nil, nil, "InvalidMountPath", fmt.Sprintf("Mount %s has an invalid path", mount.Name), nil
	}
	volume := corev1.Volume{Name: mount.Name}
	volumeMount := corev1.VolumeMount{Name: mount.Name, MountPath: filepath.Join(workspaceRootMountPath, cleanPath), ReadOnly: mount.ReadOnly}
	if mount.WorktreeRef != nil {
		worktree := new(repositoriesv1alpha1.Worktree)
		key := types.NamespacedName{Name: mount.WorktreeRef.Name, Namespace: namespace}
		if err := r.Get(ctx, key, worktree); err != nil {
			if errors.IsNotFound(err) {
				return volume, volumeMount, nil, nil, "WorktreeNotFound", fmt.Sprintf("Mounted Worktree %s does not exist", mount.WorktreeRef.Name), nil
			}
			return volume, volumeMount, nil, nil, "", "", fmt.Errorf("get mounted Worktree: %w", err)
		}
		ready := meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
		worktreeReady := ready != nil && ready.Status == metav1.ConditionTrue
		deferred := worktreebootstrap.Deferred(worktree)
		volumeReady := meta.IsStatusConditionTrue(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionVolumeReady)
		if (!worktreeReady && (!deferred || !volumeReady)) || worktree.Status.VolumeClaimName == "" {
			return volume, volumeMount, nil, nil, "WorktreeNotReady", fmt.Sprintf("Mounted Worktree %s is not ready", worktree.Name), nil
		}
		volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: worktree.Status.VolumeClaimName, ReadOnly: mount.ReadOnly}
		cleanWorktreePath := filepath.Clean(worktree.Status.WorktreePath)
		if cleanWorktreePath != repositoryRootMountPath {
			volumeMount.SubPath = strings.TrimPrefix(cleanWorktreePath, repositoryRootMountPath+"/")
		}
		var initializer *workspaceInitializer
		if deferred && !worktreeReady {
			initializer = &workspaceInitializer{
				name:   worktreebootstrap.ContainerName(worktree.Namespace, worktree.Name, worktree.UID),
				image:  r.RunnerImage,
				action: worktreebootstrap.Action(worktree.Spec.Branch, volumeMount.MountPath),
			}
		}
		if !mount.ReadOnly {
			identity := string(worktree.UID)
			if identity == "" {
				identity = namespace + "/" + worktree.Name
			}
			sum := sha256.Sum256([]byte(identity))
			claim := &workspaceWriteClaim{leaseName: workspaceWriteClaimPrefix + hex.EncodeToString(sum[:10]), worktree: worktree.Name}
			return volume, volumeMount, claim, initializer, "", "", nil
		}

		return volume, volumeMount, nil, initializer, "", "", nil
	}
	if mount.RepositoryRef != nil {
		repository := new(repositoriesv1alpha1.Repository)
		key := types.NamespacedName{Name: mount.RepositoryRef.Name, Namespace: namespace}
		if err := r.Get(ctx, key, repository); err != nil {
			if errors.IsNotFound(err) {
				return volume, volumeMount, nil, nil, "RepositoryNotFound", fmt.Sprintf("Mounted Repository %s does not exist", mount.RepositoryRef.Name), nil
			}
			return volume, volumeMount, nil, nil, "", "", fmt.Errorf("get mounted Repository: %w", err)
		}
		ready := meta.FindStatusCondition(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady)
		if ready == nil || ready.Status != metav1.ConditionTrue || repository.Status.VolumeClaimName == "" {
			return volume, volumeMount, nil, nil, "RepositoryNotReady", fmt.Sprintf("Mounted Repository %s is not ready", repository.Name), nil
		}
		volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: repository.Status.VolumeClaimName, ReadOnly: true}
		volumeMount.ReadOnly = true

		return volume, volumeMount, nil, nil, "", "", nil
	}

	return volume, volumeMount, nil, nil, "InvalidMount", fmt.Sprintf("Mount %s has no source", mount.Name), nil
}

func workspaceHomeVolumeClaim(workspace *workspacesv1alpha1.Workspace, resolved *resolvedWorkspace) *corev1.PersistentVolumeClaim {
	volumeMode := resolved.storage.VolumeModeOrDefault()
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workspace.Name,
			Namespace: workspace.Namespace,
			Labels:    map[string]string{workspaceManagedByLabel: workspace.Name},
			Annotations: map[string]string{
				workspaceImageAnnotation:    resolved.image,
				workspaceRevisionAnnotation: strconv.FormatInt(resolved.revision, 10),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      resolved.storage.AccessModesOrDefault(),
			StorageClassName: storageClassNamePointer(resolved.storage.StorageClassName),
			VolumeMode:       &volumeMode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resolved.storage.Size},
			},
		},
	}
	if resolved.sourceClaimName != "" {
		claim.Spec.DataSource = &corev1.TypedLocalObjectReference{Kind: persistentVolumeClaimKind, Name: resolved.sourceClaimName}
	}

	return claim
}

func workspaceRuntimePod(workspace *workspacesv1alpha1.Workspace, resolved *resolvedWorkspace) (*corev1.Pod, error) {
	topology, err := workspaceTopologyHash(workspace, resolved)
	if err != nil {
		return nil, err
	}
	writeClaimNames := make([]string, 0, len(resolved.writeClaims))
	for _, claim := range resolved.writeClaims {
		writeClaimNames = append(writeClaimNames, claim.leaseName)
	}
	writeClaims, err := json.Marshal(writeClaimNames)
	if err != nil {
		return nil, fmt.Errorf("marshal Workspace write claims: %w", err)
	}
	allowPrivilegeEscalation := false
	readOnlyRootFilesystem := false
	automount := resolved.automountSAToken
	volumes := make([]corev1.Volume, 0, 1+len(resolved.volumes)+1)
	volumes = append(volumes, corev1.Volume{
		Name: workspaceHomeVolumeName,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: workspace.Name,
		}},
	})
	volumes = append(volumes, resolved.volumes...)
	volumes = append(volumes, corev1.Volume{Name: workspaceRuntimeVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	volumeMounts := make([]corev1.VolumeMount, 0, 1+len(resolved.volumeMounts)+1)
	volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: workspaceHomeVolumeName, MountPath: workspaceHomeMountPath})
	volumeMounts = append(volumeMounts, resolved.volumeMounts...)
	volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: workspaceRuntimeVolumeName, MountPath: "/run/rc"})
	initContainers := make([]corev1.Container, 0, len(resolved.initializers))
	for _, initializer := range resolved.initializers {
		if initializer.image == "" {
			return nil, fmt.Errorf("workspace initializer %s has no runtime image", initializer.name)
		}
		encoded, err := lifecycle.Encode([]lifecycle.Action{initializer.action})
		if err != nil {
			return nil, fmt.Errorf("encode Workspace initializer %s: %w", initializer.name, err)
		}
		initContainers = append(initContainers, corev1.Container{
			Name: initializer.name, Image: initializer.image,
			Command:      []string{runtimeContainerName, "lifecycle", "--actions", encoded},
			VolumeMounts: volumeMounts,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &allowPrivilegeEscalation,
				ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allLinuxCapabilities}},
			},
		})
	}
	var containerLifecycle *corev1.Lifecycle
	if len(resolved.beforeStop) > 0 {
		encoded, err := lifecycle.Encode(resolved.beforeStop)
		if err != nil {
			return nil, fmt.Errorf("encode Workspace beforeStop actions: %w", err)
		}
		containerLifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{
			Command: []string{runtimeContainerName, "lifecycle", "--actions", encoded},
		}}}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workspace.Name,
			Namespace: workspace.Namespace,
			Labels:    map[string]string{workspaceManagedByLabel: workspace.Name},
			Annotations: map[string]string{
				workspaceTopologyAnnotation:      topology,
				workspaceWriteClaimsAnnotation:   string(writeClaims),
				workspaceRuntimePolicyAnnotation: workspaceRuntimePolicyVersion,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:           resolved.serviceAccount,
			AutomountServiceAccountToken: &automount,
			RestartPolicy:                corev1.RestartPolicyAlways,
			SecurityContext:              runtimepolicy.AgentPodSecurityContext(),
			NodeSelector:                 workspace.Spec.NodeSelector,
			Tolerations:                  workspace.Spec.Tolerations,
			Affinity:                     workspace.Spec.Affinity,
			RuntimeClassName:             workspace.Spec.RuntimeClassName,
			InitContainers:               initContainers,
			Containers: []corev1.Container{{
				Name:         runtimeContainerName,
				Image:        resolved.image,
				Command:      []string{runtimeContainerName, runtimeServeArgument},
				Args:         []string{"--socket", "/run/rc/rc-kube.sock", "--state-dir", "/home/agent/.rc/processes"},
				Resources:    workspace.Spec.Resources,
				VolumeMounts: volumeMounts,
				Lifecycle:    containerLifecycle,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &allowPrivilegeEscalation,
					ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allLinuxCapabilities}},
				},
			}},
			Volumes: volumes,
		},
	}, nil
}

func workspacePodWriteClaims(pod *corev1.Pod) ([]workspaceWriteClaim, error) {
	encoded := pod.Annotations[workspaceWriteClaimsAnnotation]
	if encoded == "" {
		return nil, nil
	}
	names := make([]string, 0)
	if err := json.Unmarshal([]byte(encoded), &names); err != nil {
		return nil, fmt.Errorf("decode Workspace runtime Pod write claims: %w", err)
	}
	claims := make([]workspaceWriteClaim, 0, len(names))
	for _, name := range names {
		claims = append(claims, workspaceWriteClaim{leaseName: name})
	}
	return claims, nil
}

func workspaceTopologyHash(workspace *workspacesv1alpha1.Workspace, resolved *resolvedWorkspace) (string, error) {
	topology := struct {
		RuntimePolicy    string
		Image            string
		Mounts           []workspacesv1alpha1.WorkspaceMount
		ConfigMapRefs    []workspacesv1alpha1.LocalReference
		SecretRefs       []workspacesv1alpha1.LocalReference
		AgentCredentials []workspacesv1alpha1.LocalReference
		Credentials      []workspacesv1alpha1.LocalReference
		ServiceAccount   string
		AutomountSAToken bool
		Resources        corev1.ResourceRequirements
		NodeSelector     map[string]string
		Tolerations      []corev1.Toleration
		Affinity         *corev1.Affinity
		RuntimeClassName *string
		Lifecycle        *workspacesv1alpha1.WorkspaceLifecycle
	}{
		RuntimePolicy: workspaceRuntimePolicyVersion,
		Image:         resolved.image, Mounts: workspace.Spec.Mounts, ConfigMapRefs: workspace.Spec.ConfigMapRefs,
		SecretRefs: workspace.Spec.SecretRefs, AgentCredentials: workspace.Spec.AgentCredentialRefs,
		Credentials: workspace.Spec.CredentialRefs, ServiceAccount: resolved.serviceAccount,
		AutomountSAToken: resolved.automountSAToken, Resources: workspace.Spec.Resources,
		NodeSelector: workspace.Spec.NodeSelector, Tolerations: workspace.Spec.Tolerations,
		Affinity: workspace.Spec.Affinity, RuntimeClassName: workspace.Spec.RuntimeClassName, Lifecycle: workspace.Spec.Lifecycle,
	}
	data, err := json.Marshal(topology)
	if err != nil {
		return "", fmt.Errorf("marshal Workspace topology: %w", err)
	}
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

func (r *WorkspaceReconciler) processState(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (bool, bool, *metav1.Time, error) {
	processes := new(workspacesv1alpha1.AgentProcessList)
	if err := r.List(ctx, processes, client.InNamespace(workspace.Namespace)); err != nil {
		return false, false, nil, fmt.Errorf("list Workspace Agent Processes: %w", err)
	}
	hasProcesses := false
	active := false
	var lastCompletion *metav1.Time
	for index := range processes.Items {
		process := &processes.Items[index]
		if process.Spec.TargetRef.Kind != workspacesv1alpha1.AgentProcessTargetWorkspace || process.Spec.TargetRef.Name != workspace.Name {
			continue
		}
		hasProcesses = true
		if !agentProcessTerminal(process.Status.Phase) {
			active = true
		}
		if process.Status.CompletedAt != nil && (lastCompletion == nil || process.Status.CompletedAt.After(lastCompletion.Time)) {
			completion := process.Status.CompletedAt.DeepCopy()
			lastCompletion = completion
		}
	}

	return active, hasProcesses, lastCompletion, nil
}

func agentProcessTerminal(phase workspacesv1alpha1.AgentProcessPhase) bool {
	switch phase {
	case workspacesv1alpha1.AgentProcessPhaseSucceeded, workspacesv1alpha1.AgentProcessPhaseFailed, workspacesv1alpha1.AgentProcessPhaseStopped, workspacesv1alpha1.AgentProcessPhaseLost:
		return true
	default:
		return false
	}
}

func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func (r *WorkspaceReconciler) ensureWorkspaceAccess(ctx context.Context, namespace string) error {
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: defaultWorkspaceServiceAccount, Namespace: namespace}}
	if err := createIfMissing(ctx, r.Client, serviceAccount); err != nil {
		return fmt.Errorf("ensure Workspace ServiceAccount: %w", err)
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: defaultWorkspaceServiceAccount, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"workspaces.rc.ayaka.io"}, Resources: []string{"workspaceenvironments", "workspaces", "agentprocesses", "agentprocesses/status"}, Verbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete}},
			{APIGroups: []string{"repositories.rc.ayaka.io"}, Resources: []string{"repositories", "repositoryexecs", "worktrees"}, Verbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete}},
			{APIGroups: []string{"configs.rc.ayaka.io"}, Resources: []string{"credentials", "agentcredentials"}, Verbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete}},
			{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets", "pods", "pods/log", "pods/exec", "pods/portforward"}, Verbs: []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete}},
		},
	}
	if err := reconcileRole(ctx, r.Client, role); err != nil {
		return fmt.Errorf("ensure Workspace Role: %w", err)
	}
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: defaultWorkspaceServiceAccount, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: defaultWorkspaceServiceAccount},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: defaultWorkspaceServiceAccount, Namespace: namespace}},
	}
	if err := reconcileRoleBinding(ctx, r.Client, roleBinding); err != nil {
		return fmt.Errorf("ensure Workspace RoleBinding: %w", err)
	}

	return nil
}

func reconcileRole(ctx context.Context, kubeClient client.Client, desired *rbacv1.Role) error {
	current := new(rbacv1.Role)
	err := kubeClient.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if errors.IsNotFound(err) {
		return kubeClient.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if apiequality.Semantic.DeepEqual(current.Rules, desired.Rules) {
		return nil
	}
	current.Rules = desired.Rules
	return kubeClient.Update(ctx, current)
}

func reconcileRoleBinding(ctx context.Context, kubeClient client.Client, desired *rbacv1.RoleBinding) error {
	current := new(rbacv1.RoleBinding)
	err := kubeClient.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if errors.IsNotFound(err) {
		return kubeClient.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !apiequality.Semantic.DeepEqual(current.RoleRef, desired.RoleRef) {
		if err := kubeClient.Delete(ctx, current); err != nil && !errors.IsNotFound(err) {
			return err
		}
		if err := kubeClient.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}
	if apiequality.Semantic.DeepEqual(current.Subjects, desired.Subjects) {
		return nil
	}
	current.Subjects = desired.Subjects
	return kubeClient.Update(ctx, current)
}

func createIfMissing(ctx context.Context, kubeClient client.Client, object client.Object) error {
	current := object.DeepCopyObject().(client.Object)
	err := kubeClient.Get(ctx, client.ObjectKeyFromObject(object), current)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	return kubeClient.Create(ctx, object)
}

func boolPointer(value bool) *bool {
	return &value
}

func storageClassNamePointer(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

func (r *WorkspaceReconciler) setWorkspaceStatus(ctx context.Context, key types.NamespacedName, resolved *resolvedWorkspace, readyStatus metav1.ConditionStatus, reason string, message string) error {
	current := new(workspacesv1alpha1.Workspace)
	if err := r.Get(ctx, key, current); err != nil {
		return fmt.Errorf("re-fetch Workspace before status update: %w", err)
	}
	current.Status.ObservedGeneration = current.Generation
	current.Status.HomeVolumeClaimName = current.Name
	if resolved != nil {
		if current.Status.RuntimeImage == "" {
			current.Status.RuntimeImage = resolved.image
		}
		if current.Status.SourceEnvironmentRevision == 0 {
			current.Status.SourceEnvironmentRevision = resolved.revision
		}
		if readyStatus == metav1.ConditionTrue || reason == "Starting" {
			current.Status.RuntimePodName = current.Name
		} else if reason == "Suspended" {
			current.Status.RuntimePodName = ""
		}
		outdatedStatus := metav1.ConditionFalse
		outdatedReason := "Current"
		outdatedMessage := "Workspace matches its source Environment revision"
		if resolved.outdated {
			outdatedStatus = metav1.ConditionTrue
			outdatedReason = "EnvironmentAdvanced"
			outdatedMessage = "Workspace uses an older Environment revision or image"
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: workspacesv1alpha1.WorkspaceConditionOutdated, Status: outdatedStatus,
			ObservedGeneration: current.Generation, Reason: outdatedReason, Message: outdatedMessage,
		})
	}
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.WorkspaceConditionReady, Status: readyStatus,
		ObservedGeneration: current.Generation, Reason: reason, Message: message,
	})
	if err := r.Status().Update(ctx, current); err != nil {
		return fmt.Errorf("update Workspace status: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RunnerImage == "" {
		return fmt.Errorf("workspace runner image must not be empty")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.Workspace{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.Pod{}).
		Owns(&coordinationv1.Lease{}).
		Watches(&workspacesv1alpha1.AgentProcess{}, handler.EnqueueRequestsFromMapFunc(r.workspaceForProcess)).
		Watches(&workspacesv1alpha1.WorkspaceEnvironment{}, handler.EnqueueRequestsFromMapFunc(r.workspacesForEnvironment)).
		Watches(&repositoriesv1alpha1.Worktree{}, handler.EnqueueRequestsFromMapFunc(r.workspacesForWorktree)).
		Named("workspaces-workspace").
		Complete(r)
}

func (r *WorkspaceReconciler) workspaceForProcess(_ context.Context, object client.Object) []reconcile.Request {
	process, ok := object.(*workspacesv1alpha1.AgentProcess)
	if !ok || process.Spec.TargetRef.Kind != workspacesv1alpha1.AgentProcessTargetWorkspace {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: process.Spec.TargetRef.Name, Namespace: process.Namespace}}}
}

func (r *WorkspaceReconciler) workspacesForEnvironment(ctx context.Context, object client.Object) []reconcile.Request {
	workspaces := new(workspacesv1alpha1.WorkspaceList)
	if err := r.List(ctx, workspaces, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for _, workspace := range workspaces.Items {
		if workspace.Spec.EnvironmentRef != nil && workspace.Spec.EnvironmentRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&workspace)})
		}
	}

	return requests
}

func (r *WorkspaceReconciler) workspacesForWorktree(ctx context.Context, object client.Object) []reconcile.Request {
	workspaces := new(workspacesv1alpha1.WorkspaceList)
	if err := r.List(ctx, workspaces, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for _, workspace := range workspaces.Items {
		for _, mount := range workspace.Spec.Mounts {
			if mount.WorktreeRef != nil && mount.WorktreeRef.Name == object.GetName() {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&workspace)})
				break
			}
		}
	}

	return requests
}
