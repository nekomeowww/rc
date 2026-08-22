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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const runnerTestNamespace = "development"

const runnerTestImage = "workspace:test"

func TestRunnerRejectsRepositoryRequirementMissingFromExistingWorkspace(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: runnerTestNamespace},
		Status: workspacesv1alpha1.WorkspaceStatus{Conditions: []metav1.Condition{{
			Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, Reason: "WorkspaceReady",
		}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()
	runner := &Runner{Client: kubeClient}

	_, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: runnerTestNamespace, Workspace: workspace.Name,
		Repositories: []MountRequest{{Name: "rc", MountName: "rc", Path: "rc"}},
	})
	requirements.EqualError(err, `workspace "existing" does not have a mount derived from Repository "rc"`)
	worktrees := new(repositoriesv1alpha1.WorktreeList)
	requirements.NoError(kubeClient.List(context.Background(), worktrees), "list Worktrees")
	requirements.Empty(worktrees.Items, "requirements never mutate an existing Workspace")
}

func TestRunnerCreatesGeneratedWorkspaceAndWorktreeForRepository(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: "rc", Namespace: runnerTestNamespace},
		Spec: repositoriesv1alpha1.RepositorySpec{
			Remote:  repositoriesv1alpha1.RepositoryRemoteSpec{URL: "https://github.com/nekomeowww/rc"},
			Storage: repositoriesv1alpha1.RepositoryStorageSpec{StorageClassName: "clone-capable", Size: resource.MustParse("10Gi")},
		},
		Status: repositoriesv1alpha1.RepositoryStatus{
			VolumeClaimName: "rc",
			Conditions:      []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, Reason: "StorageReady"}},
		},
	}
	environment := &workspacesv1alpha1.WorkspaceEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "prepared", Namespace: runnerTestNamespace},
		Spec: workspacesv1alpha1.WorkspaceEnvironmentSpec{
			Image:   runnerTestImage,
			Storage: workspacesv1alpha1.PersistentStorageSpec{StorageClassName: "clone-capable", Size: resource.MustParse("20Gi")},
		},
		Status: workspacesv1alpha1.WorkspaceEnvironmentStatus{
			CurrentRevision: 1, CurrentImage: runnerTestImage, CurrentVolumeClaimName: "prepared-current-1",
			Conditions: []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceEnvironmentConditionReady, Status: metav1.ConditionTrue, Reason: "EnvironmentReady"}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository, environment).Build()
	runner := &Runner{Client: kubeClient, NameGenerator: func(string) string { return "codex-generated" }}

	result, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: runnerTestNamespace, Environment: environment.Name,
		Repositories: []MountRequest{{Name: repository.Name, MountName: "rc", Path: "rc"}},
	})
	requirements.NoError(err, "prepare generated Workspace")
	assertions.True(result.Created, "report generated topology")
	assertions.Equal("codex-generated", result.Workspace.Name, "use generated sortable name")
	assertions.True(result.Workspace.Spec.Generated, "mark generated Workspace")
	requirements.Len(result.Workspace.Spec.Mounts, 1, "mount generated Worktree")
	requirements.NotNil(result.Workspace.Spec.Mounts[0].WorktreeRef, "generated Repository shortcut resolves to Worktree")

	worktree := new(repositoriesv1alpha1.Worktree)
	worktreeKey := types.NamespacedName{Name: result.Workspace.Spec.Mounts[0].WorktreeRef.Name, Namespace: runnerTestNamespace}
	requirements.NoError(kubeClient.Get(context.Background(), worktreeKey, worktree), "get generated Worktree")
	assertions.Equal(repository.Name, worktree.Spec.RepositoryRef.Name, "clone selected Repository")
	assertions.Equal("rc/codex-generated/rc", worktree.Spec.Branch, "use unique rc branch")
	assertions.Equal("codex-generated", worktree.Labels[GeneratedWorkspaceLabel], "label cascade ownership")
}

func TestRunnerTreatsCredentialsAsRequirementsForExistingWorkspace(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: runnerTestNamespace},
		Status:     workspacesv1alpha1.WorkspaceStatus{Conditions: []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, Reason: "WorkspaceReady"}}},
	}
	runner := &Runner{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()}
	_, err := runner.Prepare(context.Background(), RunRequest{Namespace: workspace.Namespace, Workspace: workspace.Name, CredentialRefs: []string{"github"}})
	requirements.EqualError(err, `workspace "existing" does not reference Credential "github"`)
}

func TestRunnerRollsBackGeneratedWorktreesWhenWorkspaceCreateFails(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: "rc", Namespace: runnerTestNamespace},
		Status:     repositoriesv1alpha1.RepositoryStatus{Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, Reason: "StorageReady"}}},
	}
	conflict := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "generated-conflict", Namespace: runnerTestNamespace}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository, conflict).Build()
	runner := &Runner{Client: kubeClient, NameGenerator: func(string) string { return conflict.Name }}

	_, err := runner.Prepare(context.Background(), RunRequest{Namespace: runnerTestNamespace, Image: runnerTestImage, Repositories: []MountRequest{{Name: repository.Name}}})
	requirements.Error(err, "report Workspace create conflict")
	worktrees := new(repositoriesv1alpha1.WorktreeList)
	requirements.NoError(kubeClient.List(context.Background(), worktrees), "list Worktrees after rollback")
	requirements.Empty(worktrees.Items, "do not orphan generated Worktrees")
}
