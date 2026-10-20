package workspaces

import (
	"context"
	"sync"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// WorkspaceEnvironmentRetentionReconciler is the Environment entry point to
// the shared service; its queue contains ordinary Environment names.
type WorkspaceEnvironmentRetentionReconciler struct {
	client.Client
	// APIReader is required. It bypasses the informer cache; SetupWithManager
	// sets it from the manager.
	APIReader client.Reader
	// History prunes the editor supervisor's finished process records.
	History HistoryPruner

	retention     *historyRetention
	retentionOnce sync.Once
}

// Reconcile applies history retention on the editor's running supervisor.
func (r *WorkspaceEnvironmentRetentionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	target := new(workspacesv1alpha1.WorkspaceEnvironment)
	if err := r.Get(ctx, req.NamespacedName, target); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	r.retentionOnce.Do(func() { r.retention = newHistoryRetention(r.APIReader, r.History, nil) })
	return ctrl.Result{RequeueAfter: r.retention.reconcile(ctx, target, target.Status.EditorPodName, target.Spec.ExecutionRetention)}, nil
}

// SetupWithManager registers kind-filtered execution events and owned workers.
func (r *WorkspaceEnvironmentRetentionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.WorkspaceEnvironment{}).
		Watches(&workspacesv1alpha1.WorkspaceExec{}, handler.EnqueueRequestsFromMapFunc(retentionTargetForProcess(workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment))).
		Owns(&corev1.Pod{}).
		Named("workspaces-environment-retention").Complete(r)
}
