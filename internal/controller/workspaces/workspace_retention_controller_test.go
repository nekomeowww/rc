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
	process := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "finished", Namespace: workspace.Namespace},
		Spec: workspacesv1alpha1.AgentProcessSpec{
			TargetRef: workspacesv1alpha1.AgentProcessTargetReference{Kind: workspacesv1alpha1.AgentProcessTargetWorkspace, Name: workspace.Name},
			Command:   []string{testTrueValue},
		},
		Status: workspacesv1alpha1.AgentProcessStatus{Phase: workspacesv1alpha1.AgentProcessPhaseSucceeded, CompletedAt: &completedAt},
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
	process := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "finished", Namespace: workspace.Namespace},
		Spec: workspacesv1alpha1.AgentProcessSpec{
			TargetRef: workspacesv1alpha1.AgentProcessTargetReference{Kind: workspacesv1alpha1.AgentProcessTargetWorkspace, Name: workspace.Name},
			Command:   []string{testTrueValue},
		},
		Status: workspacesv1alpha1.AgentProcessStatus{Phase: workspacesv1alpha1.AgentProcessPhaseSucceeded, CompletedAt: &completedAt},
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

func TestAbandonedTemporaryWorkspaceDeletesWithoutAgentProcess(t *testing.T) {
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
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()
	reconciler := &WorkspaceRetentionReconciler{Client: kubeClient}

	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	requirements.NoError(err, "reconcile abandoned temporary Workspace")
	persisted := new(workspacesv1alpha1.Workspace)
	requirements.NoError(kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted), "get deleting abandoned Workspace")
	assertions.False(persisted.DeletionTimestamp.IsZero(), "request deletion without an AgentProcess")
}

func TestNewTemporaryWorkspaceWaitsForAgentProcessCreation(t *testing.T) {
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
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()
	reconciler := &WorkspaceRetentionReconciler{Client: kubeClient}

	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	requirements.NoError(err, "reconcile new temporary Workspace")
	persisted := new(workspacesv1alpha1.Workspace)
	requirements.NoError(kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted), "get retained starting Workspace")
	assertions.True(persisted.DeletionTimestamp.IsZero(), "retain Workspace while its AgentProcess is being created")
	assertions.Positive(result.RequeueAfter, "schedule abandoned Workspace collection")
}
