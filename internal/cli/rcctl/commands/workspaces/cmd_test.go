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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
)

const (
	testWorkspaceNamespace      = "development"
	testWorkspaceName           = "dev"
	testPersonalAgentCredential = "codex-personal"
	testTeamAgentCredential     = "codex-team"
	testWorkspaceCredential     = "github-ssh"
	testMountSource             = "source"
	testWorkspaceUID            = "workspace-uid"
	testWorktreeUID             = "worktree-uid"
	testWorktreeReadyReason     = "WorktreeReady"
	testReadyWorktreeName       = "ready"
)

func TestSetWorkspaceCredentialReferences(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register config API types")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&configsv1alpha1.AgentCredential{ObjectMeta: metav1.ObjectMeta{Name: testPersonalAgentCredential, Namespace: testWorkspaceNamespace}},
		&configsv1alpha1.AgentCredential{ObjectMeta: metav1.ObjectMeta{Name: testTeamAgentCredential, Namespace: testWorkspaceNamespace}},
		&configsv1alpha1.Credential{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceCredential, Namespace: testWorkspaceNamespace}},
	).Build()
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace}}

	err := setWorkspaceCredentialReferences(
		context.Background(), kubeClient, workspace,
		[]string{testPersonalAgentCredential, testTeamAgentCredential}, []string{testWorkspaceCredential},
	)
	require.NoError(t, err, "attach existing credentials to a Workspace")

	assert.Equal(t, []workspacesv1alpha1.LocalReference{{Name: testPersonalAgentCredential}, {Name: testTeamAgentCredential}}, workspace.Spec.AgentCredentialRefs, "preserve AgentCredential order")
	assert.Equal(t, []workspacesv1alpha1.LocalReference{{Name: testWorkspaceCredential}}, workspace.Spec.CredentialRefs, "attach generic Credentials")
}

func TestSetWorkspaceCredentialReferencesRejectsMissingResource(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register config API types")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	t.Run("AgentCredential", func(t *testing.T) {
		workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace}}
		err := setWorkspaceCredentialReferences(context.Background(), kubeClient, workspace, []string{"missing"}, nil)

		require.Error(t, err, "reject a missing AgentCredential")
		assert.ErrorContains(t, err, `get AgentCredential "missing"`, "identify the missing AgentCredential")
		assert.Empty(t, workspace.Spec.AgentCredentialRefs, "do not partially attach AgentCredentials")
	})

	t.Run("Credential", func(t *testing.T) {
		workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace}}
		err := setWorkspaceCredentialReferences(context.Background(), kubeClient, workspace, nil, []string{"missing"})

		require.Error(t, err, "reject a missing Credential")
		assert.ErrorContains(t, err, `get Credential "missing"`, "identify the missing Credential")
		assert.Empty(t, workspace.Spec.CredentialRefs, "do not partially attach Credentials")
	})
}

func TestWorkspaceListItemsSortsByName(t *testing.T) {
	t.Parallel()
	items := workspaceListItems([]workspacesv1alpha1.Workspace{
		{ObjectMeta: metav1.ObjectMeta{Name: "zeta"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}},
	})

	require.Len(t, items, 2, "retain every Workspace")
	assert.Equal(t, "alpha", items[0].Name, "sort inventory by name")
	assert.Equal(t, "zeta", items[1].Name, "retain the second sorted Workspace")
}

func TestWorkspaceListTableUsesHumanAgeAndWideMetadata(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	table := workspaceListTable([]workspacesv1alpha1.Workspace{{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Hour))},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState: workspacesv1alpha1.WorkspaceDesiredStateRunning, Generated: true,
		},
	}}, now)

	require.Len(t, table.Rows, 1, "create one row per Workspace")
	assert.Equal(t, "120m", table.Rows[0][7], "use a compact Kubernetes-style age")
	assert.True(t, table.Columns[3].Wide, "keep generated metadata in wide output")
}

func TestWorkspaceListAndGetCommandsExposeOutputFormats(t *testing.T) {
	t.Parallel()
	listCommand := newListCommand(kubeconfig.NewFlags())
	getCommand := newGetCommand(kubeconfig.NewFlags())

	assert.Contains(t, listCommand.Aliases, "ls", "offer the conventional list alias")
	require.NotNil(t, listCommand.Flag("output"), "list accepts an output format")
	require.NotNil(t, getCommand.Flag("output"), "get accepts a structured output format")
	require.NoError(t, getCommand.Args(getCommand, []string{testWorkspaceName}), "get accepts exactly one Workspace name")
	require.Error(t, getCommand.Args(getCommand, nil), "get requires a Workspace name")
}

func TestApplyWorkspaceMountValidatesConflictsBeforeStoppingProcesses(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace},
		Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{
			Name: testMountSource, Path: testMountSource, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: testMountSource},
		}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()
	stopCalls := 0

	_, err := applyWorkspaceMount(context.Background(), kubeClient, types.NamespacedName{
		Name: workspace.Name, Namespace: workspace.Namespace,
	}, workspacesv1alpha1.WorkspaceMount{
		Name: testMountSource, Path: "other", WorktreeRef: &workspacesv1alpha1.LocalReference{Name: "other"},
	}, func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) {
		stopCalls++
		return []string{"process"}, nil
	})

	requirements.Error(err, "reject a duplicate mount name")
	assertions.ErrorContains(err, "already exists", "identify the mount conflict")
	assertions.Zero(stopCalls, "do not stop processes before validation succeeds")
	persisted := new(workspacesv1alpha1.Workspace)
	requirements.NoError(kubeClient.Get(context.Background(), types.NamespacedName{
		Name: workspace.Name, Namespace: workspace.Namespace,
	}, persisted), "read Workspace after rejected mount")
	assertions.Len(persisted.Spec.Mounts, 1, "leave Workspace topology unchanged")
}

func TestWorkspaceReadyForGenerationRejectsStaleReadyCondition(t *testing.T) {
	t.Parallel()
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: workspacesv1alpha1.WorkspaceStatus{
			ObservedGeneration: 1,
			Conditions: []metav1.Condition{{
				Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue,
				ObservedGeneration: 1, Reason: "WorkspaceReady",
			}},
		},
	}

	assert.False(t, workspaceReadyForGeneration(workspace, 2), "reject Ready=True from the previous topology")
	workspace.Status.ObservedGeneration = 2
	assert.False(t, workspaceReadyForGeneration(workspace, 2), "also require the Ready condition to observe the topology")
	workspace.Status.Conditions[0].ObservedGeneration = 2
	assert.True(t, workspaceReadyForGeneration(workspace, 2), "accept Ready=True for the requested topology")
}

func TestApplyWorktreeMountValidatesWorktreeBeforeStoppingProcesses(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()
	stopCalls := 0

	_, err := applyWorktreeMount(context.Background(), kubeClient, workspace, workspace.Namespace, "missing", mountOptions{},
		func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) {
			stopCalls++
			return []string{"process"}, nil
		})

	requirements.Error(err, "reject a missing Worktree")
	assertions.ErrorContains(err, "get Worktree", "identify Worktree lookup failure")
	assertions.Zero(stopCalls, "do not stop processes before Worktree validation succeeds")
}

func TestApplyWorkspaceMountValidatesPathBeforeStoppingProcesses(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()
	stopCalls := 0

	_, err := applyWorkspaceMount(context.Background(), kubeClient, types.NamespacedName{Name: workspace.Name, Namespace: workspace.Namespace},
		workspacesv1alpha1.WorkspaceMount{Name: "invalid", Path: "/absolute", WorktreeRef: &workspacesv1alpha1.LocalReference{Name: "tree"}},
		func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) {
			stopCalls++
			return nil, nil
		})

	require.Error(t, err, "reject an invalid mount path")
	assert.Zero(t, stopCalls, "do not stop processes before path validation succeeds")
}

func TestApplyWorktreeMountValidatesReadyBeforeStoppingProcesses(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace, UID: testWorkspaceUID}}
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: "not-ready", Namespace: testWorkspaceNamespace}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, worktree).Build()
	stopCalls := 0

	_, err := applyWorktreeMount(context.Background(), kubeClient, workspace, workspace.Namespace, worktree.Name, mountOptions{},
		func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) {
			stopCalls++
			return nil, nil
		})

	require.Error(t, err, "reject a Worktree without a ready child volume")
	assert.ErrorContains(t, err, "is not Ready", "identify Worktree readiness")
	assert.Zero(t, stopCalls, "do not stop processes before Worktree readiness validation succeeds")
}

func TestApplyWorktreeMountValidatesWriteLeaseBeforeStoppingProcesses(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	require.NoError(t, coordinationv1.AddToScheme(scheme), "register coordination API types")
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace, UID: testWorkspaceUID}}
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: "in-use", Namespace: testWorkspaceNamespace, UID: testWorktreeUID, Generation: 1},
		Status: repositoriesv1alpha1.WorktreeStatus{ObservedGeneration: 1, VolumeClaimName: "in-use-pvc", Conditions: []metav1.Condition{{
			Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: testWorktreeReadyReason,
		}}},
	}
	foreignHolder := "other-writer-uid"
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: worktreeclaim.LeaseName(worktree), Namespace: worktree.Namespace, Labels: map[string]string{worktreeclaim.HolderLabel: "other-writer"}},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &foreignHolder},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, worktree, lease).Build()
	stopCalls := 0

	_, err := applyWorktreeMount(context.Background(), kubeClient, workspace, workspace.Namespace, worktree.Name, mountOptions{},
		func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) {
			stopCalls++
			return nil, nil
		})

	require.Error(t, err, "reject a Worktree held by another writer")
	assert.ErrorContains(t, err, "other-writer", "identify the active writer")
	assert.Zero(t, stopCalls, "do not stop processes before Worktree Lease validation succeeds")
}

func TestApplyWorktreeMountRejectsStaleReadyBeforeStoppingProcesses(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace, UID: testWorkspaceUID}}
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: "stale-ready", Namespace: testWorkspaceNamespace, UID: testWorktreeUID, Generation: 2},
		Status: repositoriesv1alpha1.WorktreeStatus{ObservedGeneration: 1, VolumeClaimName: "stale-pvc", Conditions: []metav1.Condition{{
			Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: testWorktreeReadyReason,
		}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, worktree).Build()
	stopCalls := 0

	_, err := applyWorktreeMount(context.Background(), kubeClient, workspace, workspace.Namespace, worktree.Name, mountOptions{},
		func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) {
			stopCalls++
			return nil, nil
		})

	require.Error(t, err, "reject Ready=True from an older Worktree generation")
	assert.ErrorContains(t, err, "is not Ready", "identify stale Worktree readiness")
	assert.Zero(t, stopCalls, "do not stop processes for stale Worktree status")
}

func TestApplyWorkspaceMountPreservesPartiallyStoppedProcesses(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	require.NoError(t, coordinationv1.AddToScheme(scheme), "register coordination API types")
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace, UID: testWorkspaceUID}}
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: testReadyWorktreeName, Namespace: testWorkspaceNamespace, UID: testWorktreeUID},
		Status: repositoriesv1alpha1.WorktreeStatus{VolumeClaimName: "ready-pvc", Conditions: []metav1.Condition{{
			Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue, Reason: testWorktreeReadyReason,
		}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, worktree).Build()
	stopErr := errors.New("second process could not stop")

	result, err := applyWorkspaceMount(context.Background(), kubeClient, types.NamespacedName{Name: workspace.Name, Namespace: workspace.Namespace},
		workspacesv1alpha1.WorkspaceMount{Name: testReadyWorktreeName, Path: testReadyWorktreeName, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}},
		func(context.Context, *workspacesv1alpha1.Workspace) ([]string, error) {
			return []string{"first-process"}, stopErr
		})

	require.ErrorIs(t, err, stopErr)
	assert.Equal(t, []string{"first-process"}, result.stoppedProcesses, "preserve successful stops for caller reporting")
	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKeyFromObject(workspace), persisted))
	assert.Empty(t, persisted.Spec.Mounts, "roll back topology when process shutdown is incomplete")
	lease := new(coordinationv1.Lease)
	err = kubeClient.Get(context.Background(), types.NamespacedName{Name: worktreeclaim.LeaseName(worktree), Namespace: worktree.Namespace}, lease)
	assert.True(t, apierrors.IsNotFound(err), "release a newly reserved Lease after rollback")
}

func TestApplyWorkspaceMountReservesWriteLeaseBeforeStoppingProcesses(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	require.NoError(t, coordinationv1.AddToScheme(scheme), "register coordination API types")
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace, UID: testWorkspaceUID}}
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: testReadyWorktreeName, Namespace: testWorkspaceNamespace, UID: testWorktreeUID},
		Status: repositoriesv1alpha1.WorktreeStatus{VolumeClaimName: "ready-pvc", Conditions: []metav1.Condition{{
			Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue, Reason: testWorktreeReadyReason,
		}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, worktree).Build()
	mount := workspacesv1alpha1.WorkspaceMount{Name: worktree.Name, Path: worktree.Name, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}}

	_, err := applyWorkspaceMount(context.Background(), kubeClient, client.ObjectKeyFromObject(workspace), mount,
		func(ctx context.Context, current *workspacesv1alpha1.Workspace) ([]string, error) {
			lease := new(coordinationv1.Lease)
			require.NoError(t, kubeClient.Get(ctx, types.NamespacedName{Name: worktreeclaim.LeaseName(worktree), Namespace: worktree.Namespace}, lease), "reserve the write Lease before invoking the stopper")
			assert.Equal(t, string(workspace.UID), *lease.Spec.HolderIdentity)
			persisted := new(workspacesv1alpha1.Workspace)
			require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted))
			assert.Equal(t, []workspacesv1alpha1.WorkspaceMount{mount}, persisted.Spec.Mounts, "persist topology intent before stopping processes")
			return nil, nil
		})

	require.NoError(t, err)
}
