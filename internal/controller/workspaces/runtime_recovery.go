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

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// reconcileRuntimeRecovery runs before dependency resolution: losing a Worktree
// or Environment must not hide a terminal runtime or block its cleanup.
//
// Call stack:
// Reconcile -> reconcileRuntimeRecovery -> planTerminalRuntime
//
//	-> persistTerminalRuntimePlan -> removeHotMounts -> Delete (UID precondition)
func (r *WorkspaceReconciler) reconcileRuntimeRecovery(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (ctrl.Result, bool, error) {
	if workspace.Spec.DesiredState == workspacesv1alpha1.WorkspaceDesiredStateSuspended {
		return ctrl.Result{}, false, nil
	}
	pod := new(corev1.Pod)
	err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(workspace), pod)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, false, nil
	} else if err != nil {
		return ctrl.Result{}, true, err
	}
	if !metav1.IsControlledBy(pod, workspace) || !runtimePodTerminal(pod) {
		return ctrl.Result{}, false, nil
	}
	processes := new(workspacesv1alpha1.WorkspaceExecList)
	if err := r.APIReader.List(ctx, processes, client.InNamespace(workspace.Namespace)); err != nil {
		return ctrl.Result{}, true, err
	}
	bound := false
	for i := range processes.Items {
		process := &processes.Items[i]
		if process.Status.RuntimePodName == pod.Name && process.Status.RuntimePodUID == string(pod.UID) && !process.Status.Phase.Terminal() {
			bound = true
			break
		}
	}
	plan := planTerminalRuntime(pod, bound)
	if err := r.persistTerminalRuntimePlan(ctx, workspace, plan); err != nil {
		return ctrl.Result{}, true, err
	}
	result := ctrl.Result{RequeueAfter: workspaceDependencyRequeue}
	if !plan.replace {
		return result, true, nil
	}
	// Do not release write reservations until the old runtime AND every helper
	// have disappeared. Failed helpers require their existing node cleanup Pods.
	removed, err := r.removeHotMounts(ctx, workspace)
	if err != nil {
		return result, true, err
	}
	if !removed {
		plan.message += "; waiting for old Worktree mounts to unmount"
		return result, true, r.persistTerminalRuntimePlan(ctx, workspace, plan)
	}
	if pod.DeletionTimestamp.IsZero() {
		err = r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID})
		if err != nil && !apierrors.IsNotFound(err) {
			return result, true, fmt.Errorf("delete terminal Workspace runtime Pod: %w", err)
		}
	}
	return result, true, nil
}

// persistTerminalRuntimePlan records the real exit diagnosis before cleanup.
// No retry history is needed: the Pod and executions are re-observed on restart.
func (r *WorkspaceReconciler) persistTerminalRuntimePlan(ctx context.Context, workspace *workspacesv1alpha1.Workspace, plan *terminalRuntimePlan) error {
	current := new(workspacesv1alpha1.Workspace)
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		return err
	}
	previous := current.DeepCopy()
	current.Status.ObservedGeneration = current.Generation
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionFalse,
		ObservedGeneration: current.Generation, Reason: plan.reason, Message: plan.message,
	})
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.WorkspaceConditionDegraded, Status: metav1.ConditionTrue,
		ObservedGeneration: current.Generation, Reason: plan.reason, Message: plan.message,
	})
	if apiequality.Semantic.DeepEqual(previous.Status, current.Status) {
		return nil
	}
	return r.Status().Update(ctx, current)
}
