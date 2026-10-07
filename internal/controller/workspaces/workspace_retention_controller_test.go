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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func TestTemporaryWorkspaceDeletionDoesNotDependOnRuntimeTopology(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	scheme := runtime.NewScheme()
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	completedAt := metav1.NewTime(time.Now().Add(-temporaryWorkspaceCleanupDelay - time.Second))
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "temporary", Namespace: testNamespace, Finalizers: []string{workspaceFinalizer}},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			RetentionPolicy: workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit,
			EnvironmentRef: &workspacesv1alpha1.LocalReference{
				Name: "missing-environment",
			},
		},
	}
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "finished", Namespace: workspace.Namespace},
		Spec: workspacesv1alpha1.WorkspaceExecSpec{
			TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name},
			Command:   []string{testTrueValue},
		},
		Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, CompletedAt: &completedAt},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace, process).WithObjects(workspace, process).Build()
	reconciler := &WorkspaceRetentionReconciler{Client: kubeClient}

	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	requirements.NoError(err, "reconcile temporary Workspace with missing runtime dependency")
	persisted := new(workspacesv1alpha1.Workspace)
	requirements.NoError(kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted), "get deleting temporary Workspace")
	assertions.False(persisted.DeletionTimestamp.IsZero(), "request deletion without resolving runtime topology")
}

func TestTemporaryWorkspaceWaitsDuringTerminalGracePeriod(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	scheme := runtime.NewScheme()
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	completedAt := metav1.Now()
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "observing", Namespace: testNamespace, Finalizers: []string{workspaceFinalizer}},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			RetentionPolicy: workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit,
		},
	}
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "finished", Namespace: workspace.Namespace},
		Spec: workspacesv1alpha1.WorkspaceExecSpec{
			TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name},
			Command:   []string{testTrueValue},
		},
		Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded, CompletedAt: &completedAt},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace, process).WithObjects(workspace, process).Build()
	reconciler := &WorkspaceRetentionReconciler{Client: kubeClient}

	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	requirements.NoError(err, "reconcile recently completed temporary Workspace")
	persisted := new(workspacesv1alpha1.Workspace)
	requirements.NoError(kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted), "get retained temporary Workspace")
	assertions.True(persisted.DeletionTimestamp.IsZero(), "retain Workspace while clients observe the terminal result")
	assertions.Positive(result.RequeueAfter, "schedule cleanup after the terminal grace period")
}

func TestAbandonedTemporaryWorkspaceDeletesWithoutWorkspaceExec(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	scheme := runtime.NewScheme()
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "abandoned", Namespace: testNamespace, Finalizers: []string{workspaceFinalizer},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			RetentionPolicy: workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit,
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(workspace).Build()
	reconciler := &WorkspaceRetentionReconciler{Client: kubeClient}

	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	requirements.NoError(err, "reconcile abandoned temporary Workspace")
	persisted := new(workspacesv1alpha1.Workspace)
	requirements.NoError(kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted), "get deleting abandoned Workspace")
	assertions.False(persisted.DeletionTimestamp.IsZero(), "request deletion without an WorkspaceExec")
}

func TestNewTemporaryWorkspaceWaitsForWorkspaceExecCreation(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	scheme := runtime.NewScheme()
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "starting", Namespace: testNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			RetentionPolicy: workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit,
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(workspace).Build()
	reconciler := &WorkspaceRetentionReconciler{Client: kubeClient}

	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	requirements.NoError(err, "reconcile new temporary Workspace")
	persisted := new(workspacesv1alpha1.Workspace)
	requirements.NoError(kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted), "get retained starting Workspace")
	assertions.True(persisted.DeletionTimestamp.IsZero(), "retain Workspace while its WorkspaceExec is being created")
	assertions.Positive(result.RequeueAfter, "schedule abandoned Workspace collection")
}

// ROOT CAUSE: Suspension stopped compute but had no storage-retention stage.
func TestSuspendedWorkspaceExpires(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{}
	// Decode the proposed policy through the API boundary: before the change the
	// field is silently discarded and the retained Workspace is never collected.
	require.NoError(t, json.Unmarshal([]byte(`{"spec":{"desiredState":"Suspended","deleteAfterSuspended":"1h"}}`), workspace))
	workspace.ObjectMeta = metav1.ObjectMeta{Name: "suspended", Namespace: testNamespace, Finalizers: []string{workspaceFinalizer}}
	workspace.Status.SuspendedAt = &metav1.Time{Time: time.Now().Add(-2 * time.Hour)}
	workspace.Status.Conditions = []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionFalse, Reason: reasonSuspended, LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Hour))}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(workspace).Build()
	_, err := (&WorkspaceRetentionReconciler{Client: kubeClient}).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	require.NoError(t, err)
	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted))
	assert.False(t, persisted.DeletionTimestamp.IsZero(), "collect storage after the configured suspended interval")
}
