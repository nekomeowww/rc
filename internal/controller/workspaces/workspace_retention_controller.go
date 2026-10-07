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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	corev1 "k8s.io/api/core/v1"
)

const (
	temporaryWorkspaceCleanupDelay = 5 * time.Minute
	temporaryWorkspaceStartTimeout = 15 * time.Minute
)

// WorkspaceRetentionReconciler deletes Workspaces whose retention policy has
// expired independently of runtime topology health.
type WorkspaceRetentionReconciler struct {
	client.Client
	// APIReader bypasses informer lag for destructive retention decisions.
	APIReader client.Reader
	Runtime   processruntime.Runtime
}

// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaceexecs,verbs=get;list;watch

func (r *WorkspaceRetentionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	workspace := new(workspacesv1alpha1.Workspace)
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if !workspace.Spec.IsTemporary() {
		return (&executionRetentionService{Client: r.Client, APIReader: r.APIReader, Runtime: r.Runtime}).reconcileTarget(ctx, workspace, workspace.Spec.ExecutionRetention)
	}
	// Whole-target expiry takes precedence over any per-transcript failure.
	result, err := r.reconcileTemporaryWorkspace(ctx, workspace)
	if err != nil {
		return result, err
	}
	cleanupErr := (&executionRetentionService{Client: r.Client, APIReader: r.APIReader, Runtime: r.Runtime}).reconcileTranscripts(ctx, workspace, nil)
	return result, cleanupErr
}

func (r *WorkspaceRetentionReconciler) reconcileTemporaryWorkspace(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (ctrl.Result, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := new(workspacesv1alpha1.Workspace)
	if err := reader.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if current.UID != workspace.UID || !current.DeletionTimestamp.IsZero() || !current.Spec.IsTemporary() {
		return ctrl.Result{}, nil
	}
	active, hasProcesses, lastCompletion, err := workspaceProcessState(ctx, reader, current)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !hasProcesses {
		cleanupAt := current.CreationTimestamp.Add(temporaryWorkspaceStartTimeout)
		if remaining := time.Until(cleanupAt); remaining > 0 {
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	} else if active {
		return ctrl.Result{}, nil
	} else if lastCompletion != nil {
		cleanupAt := lastCompletion.Add(temporaryWorkspaceCleanupDelay)
		if remaining := time.Until(cleanupAt); remaining > 0 {
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}
	uid, version := current.UID, current.ResourceVersion
	if err := r.Delete(ctx, current, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete completed temporary Workspace: %w", err)
	}
	logf.FromContext(ctx).Info("Requested temporary Workspace deletion", "name", current.Name)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceRetentionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.Workspace{}).
		Watches(&workspacesv1alpha1.WorkspaceExec{}, handler.EnqueueRequestsFromMapFunc(retentionTargetForProcess(workspacesv1alpha1.WorkspaceExecTargetWorkspace))).
		Owns(&corev1.Pod{}).
		Named("workspaces-workspace-retention").
		Complete(r)
}
