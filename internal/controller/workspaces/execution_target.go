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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	executionTargetIndex = "spec.targetRef.name"
	executionPodIndex    = "status.runtimePodName"
)

func executionTargetNames(object client.Object) []string {
	return []string{object.(*workspacesv1alpha1.WorkspaceExec).Spec.TargetRef.Name}
}

func executionPodNames(object client.Object) []string {
	name := object.(*workspacesv1alpha1.WorkspaceExec).Status.RuntimePodName
	if name == "" {
		return nil
	}
	return []string{name}
}

// readExecutionTarget rejects same-name replacements rather than borrowing
// their storage or lifecycle. A nil object means the original target is gone.
func readExecutionTarget(ctx context.Context, reader client.Reader, process *workspacesv1alpha1.WorkspaceExec) (client.Object, error) {
	var target client.Object
	switch process.Spec.TargetRef.Kind {
	case workspacesv1alpha1.WorkspaceExecTargetWorkspace:
		target = new(workspacesv1alpha1.Workspace)
	case workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment:
		target = new(workspacesv1alpha1.WorkspaceEnvironment)
	default:
		return nil, nil
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: process.Namespace, Name: process.Spec.TargetRef.Name}, target); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	owner := metav1.GetControllerOf(process)
	if owner == nil || owner.APIVersion != workspacesv1alpha1.GroupVersion.String() || owner.Kind != string(process.Spec.TargetRef.Kind) || owner.Name != process.Spec.TargetRef.Name || owner.UID == "" || target.GetUID() == "" || owner.UID != target.GetUID() {
		return nil, nil
	}
	return target, nil
}

// retentionTargetForProcess maps a WorkspaceExec event to its target of kind.
func retentionTargetForProcess(kind workspacesv1alpha1.WorkspaceExecTargetKind) handler.MapFunc {
	return func(_ context.Context, object client.Object) []reconcile.Request {
		process, ok := object.(*workspacesv1alpha1.WorkspaceExec)
		if !ok || process.Spec.TargetRef.Kind != kind {
			return nil
		}
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: process.Namespace, Name: process.Spec.TargetRef.Name}}}
	}
}

// ensureExecutionCompletedAt starts the result window of a terminal record
// that has no completion time. Creation time is never used as completion.
func (r *WorkspaceExecReconciler) ensureExecutionCompletedAt(ctx context.Context, process *workspacesv1alpha1.WorkspaceExec) error {
	if process.Status.CompletedAt != nil {
		return nil
	}
	now := metav1.Now()
	process.Status.CompletedAt = &now
	return r.Status().Update(ctx, process)
}
