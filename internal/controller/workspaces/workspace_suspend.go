package workspaces

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// suspendWorkspace tears down consumers even when storage admission has closed.
// Dependency availability is required to start a runtime, not to stop it.
func (r *WorkspaceReconciler) suspendWorkspace(ctx context.Context, workspace *workspacesv1alpha1.Workspace, resolved *resolvedWorkspace, active bool) (ctrl.Result, error) {
	if active {
		return ctrl.Result{}, r.setWorkspaceStatus(ctx, client.ObjectKeyFromObject(workspace), resolved, metav1.ConditionFalse, "ActiveProcesses", "Workspace cannot suspend while processes are active")
	}
	if removed, err := r.removeHotMounts(ctx, workspace); err != nil {
		return ctrl.Result{}, err
	} else if !removed {
		return ctrl.Result{RequeueAfter: workspaceDependencyRequeue}, r.setWorkspaceStatus(ctx, client.ObjectKeyFromObject(workspace), resolved, metav1.ConditionFalse, "Unmounting", "Workspace Worktree mounts are stopping")
	}
	pod := new(corev1.Pod)
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(workspace), pod); err == nil {
		if !metav1.IsControlledBy(pod, workspace) {
			return ctrl.Result{}, r.setWorkspaceStatus(ctx, client.ObjectKeyFromObject(workspace), resolved, metav1.ConditionFalse, "RuntimePodConflict", "A Pod with the Workspace runtime name exists but is not owned by this Workspace")
		}
		if err := r.Delete(ctx, pod); err != nil {
			return ctrl.Result{}, fmt.Errorf("delete suspended Workspace runtime Pod: %w", err)
		}
		logf.FromContext(ctx).Info("Deleted Workspace runtime Pod", "name", pod.Name)
		return ctrl.Result{RequeueAfter: workspaceDependencyRequeue}, r.setWorkspaceStatus(ctx, client.ObjectKeyFromObject(workspace), resolved, metav1.ConditionFalse, "Stopping", "Workspace runtime Pod is stopping")
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get suspended Workspace runtime Pod: %w", err)
	}
	if err := r.releaseClaims(ctx, workspace); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.setWorkspaceStatus(ctx, client.ObjectKeyFromObject(workspace), resolved, metav1.ConditionFalse, reasonSuspended, "Workspace runtime is suspended")
}
