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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/rcplatform"
)

const runnerTestNamespace = "development"

const runnerTestImage = "workspace:test"

const runnerTestExistingWorkspace = "existing"

const runnerTestDefaultWorkspace = "default"

const runnerTestWorkspaceReadyReason = "WorkspaceReady"

const runnerTestStorageReadyReason = "StorageReady"

func TestRunnerRejectsRepositoryRequirementMissingFromExistingWorkspace(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: runnerTestExistingWorkspace, Namespace: runnerTestNamespace},
		Status: workspacesv1alpha1.WorkspaceStatus{Conditions: []metav1.Condition{{
			Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, Reason: runnerTestWorkspaceReadyReason,
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

func TestRunnerCreatesTemporaryWorkspaceAndWorktreeForRepository(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
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
			Conditions:      []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, Reason: runnerTestStorageReadyReason}},
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
	runner := &Runner{Client: kubeClient, NameGenerator: func(string) string { return "codex-temporary" }}
	gpuResource := corev1.ResourceName("nvidia.com/gpu")

	result, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: runnerTestNamespace, Create: true, Remove: true, Environment: environment.Name,
		Repositories: []MountRequest{{Name: repository.Name, MountName: "rc", Path: "rc"}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{gpuResource: resource.MustParse("2")},
			Limits:   corev1.ResourceList{gpuResource: resource.MustParse("2")},
		},
	})
	requirements.NoError(err, "prepare temporary Workspace")
	assertions.True(result.Created, "report created topology")
	assertions.Equal("codex-temporary", result.Workspace.Name, "use generated sortable name")
	assertions.Equal(workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit, result.Workspace.Spec.RetentionPolicy, "record temporary retention")
	requestedGPU := result.Workspace.Spec.Resources.Limits[gpuResource]
	assertions.Zero(requestedGPU.Cmp(resource.MustParse("2")), "retain temporary Workspace GPU resources")
	requirements.Len(result.Workspace.Spec.Mounts, 1, "mount generated Worktree")
	requirements.NotNil(result.Workspace.Spec.Mounts[0].WorktreeRef, "generated Repository shortcut resolves to Worktree")

	worktree := new(repositoriesv1alpha1.Worktree)
	worktreeKey := types.NamespacedName{Name: result.Workspace.Spec.Mounts[0].WorktreeRef.Name, Namespace: runnerTestNamespace}
	requirements.NoError(kubeClient.Get(context.Background(), worktreeKey, worktree), "get generated Worktree")
	assertions.Equal(repository.Name, worktree.Spec.RepositoryRef.Name, "clone selected Repository")
	assertions.Equal("rc/codex-temporary/rc", worktree.Spec.Branch, "use unique rc branch")
	assertions.Equal("codex-temporary", worktree.Labels[CreatedForWorkspaceLabel], "label cascade ownership")
}

func TestRunnerMarksTemporaryWorkspaceAndOwnsGeneratedWorktree(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: "rc", Namespace: runnerTestNamespace},
		Status: repositoriesv1alpha1.RepositoryStatus{
			Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, Reason: runnerTestStorageReadyReason}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository).Build()
	runner := &Runner{Client: kubeClient, NameGenerator: func(string) string { return "codex-temporary" }}

	target, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: runnerTestNamespace, Create: true, Remove: true, Image: runnerTestImage,
		Repositories: []MountRequest{{Name: repository.Name}},
	})
	requirements.NoError(err, "prepare temporary Workspace")
	assertions.Equal(workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit, target.Workspace.Spec.RetentionPolicy, "record automatic cleanup semantics")
	requirements.Len(target.Workspace.Spec.Mounts, 1, "mount generated Worktree")
	requirements.NotNil(target.Workspace.Spec.Mounts[0].WorktreeRef, "temporary Repository shortcut resolves to Worktree")

	worktree := new(repositoriesv1alpha1.Worktree)
	key := types.NamespacedName{Name: target.Workspace.Spec.Mounts[0].WorktreeRef.Name, Namespace: runnerTestNamespace}
	requirements.NoError(kubeClient.Get(context.Background(), key, worktree), "get generated Worktree")
	requirements.Len(worktree.OwnerReferences, 1, "temporary Workspace owns its generated Worktree")
	assertions.Equal(target.Workspace.Name, worktree.OwnerReferences[0].Name, "cascade generated Worktree cleanup")
	requirements.NotNil(worktree.OwnerReferences[0].Controller, "generated Worktree has a controller owner")
	assertions.True(*worktree.OwnerReferences[0].Controller, "use controller ownership for generated Worktree")
}

func TestRunnerWritesEnvironmentToTemporaryWorkspace(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	runner := &Runner{Client: kubeClient, NameGenerator: func(string) string { return "temporary" }}
	environment := []corev1.EnvVar{{Name: NPMRegistryEnvironmentName, Value: testNPMRegistryURL + "/"}, {Name: CorepackRegistryEnvironmentName, Value: testNPMRegistryURL}}

	target, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: runnerTestNamespace, Create: true, Remove: true, Image: runnerTestImage, Env: environment,
	})
	requirements.NoError(err, "prepare temporary Workspace")
	assert.Equal(t, environment, target.Workspace.Spec.Env, "put registry defaults on the temporary Workspace")
}

func TestRunnerUsesDefaultWorkspaceWhenNoCodeSourceIsSelected(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	defaultWorkspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: runnerTestDefaultWorkspace, Namespace: runnerTestNamespace},
		Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{
			Name: "rc", Path: "rc", WorktreeRef: &workspacesv1alpha1.LocalReference{Name: "rc-main"},
		}}},
		Status: workspacesv1alpha1.WorkspaceStatus{Conditions: []metav1.Condition{{
			Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, Reason: runnerTestWorkspaceReadyReason,
		}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaultWorkspace).Build()
	runner := &Runner{Client: kubeClient}

	result, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: runnerTestNamespace, DefaultWorkspace: defaultWorkspace.Name, Image: runnerTestImage,
	})
	requirements.NoError(err, "prepare mountless Workspace")
	assertions.False(result.Created, "reuse the selected default Workspace")
	assertions.Equal(defaultWorkspace.Name, result.Workspace.Name, "run without code requirements in the default Workspace")
}

func TestRunnerRequiresWorkspaceSelectionWithoutTemporary(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	runner := &Runner{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	_, err := runner.Prepare(context.Background(), RunRequest{Namespace: runnerTestNamespace})

	requirements.EqualError(err, "select an existing Workspace with exec or create one with run")
}

func TestRunnerRejectsWorkspaceSelectionWithTemporary(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	runner := &Runner{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	_, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: runnerTestNamespace, Workspace: runnerTestExistingWorkspace, Create: true, Remove: true,
	})

	requirements.EqualError(err, "workspace selection and creation are mutually exclusive")
}

func TestRunnerRejectsExistingTemporaryWorkspace(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "temporary", Namespace: runnerTestNamespace},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			RetentionPolicy: workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit,
		},
		Status: workspacesv1alpha1.WorkspaceStatus{Conditions: []metav1.Condition{{
			Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, Reason: runnerTestWorkspaceReadyReason,
		}}},
	}
	runner := &Runner{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()}

	_, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: runnerTestNamespace, Workspace: workspace.Name,
	})

	requirements.EqualError(err, `workspace "temporary" is temporary and cannot be selected for another run`)
}

func TestRunnerTreatsGPUResourcesAsRequirementsForExistingWorkspace(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	gpuResource := corev1.ResourceName("nvidia.com/gpu")
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: runnerTestExistingWorkspace, Namespace: runnerTestNamespace},
		Spec: workspacesv1alpha1.WorkspaceSpec{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			gpuResource: resource.MustParse("1"),
		}}},
		Status: workspacesv1alpha1.WorkspaceStatus{Conditions: []metav1.Condition{{
			Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, Reason: runnerTestWorkspaceReadyReason,
		}}},
	}
	runner := &Runner{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()}

	_, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: workspace.Namespace, Workspace: workspace.Name,
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{gpuResource: resource.MustParse("2")}},
	})
	requirements.Error(err, "reject a Workspace with insufficient GPU resources")
	assertions.EqualError(err, "workspace \"existing\" does not provide requested resource nvidia.com/gpu=2", "report the unsatisfied GPU requirement")
}

func TestRunnerTreatsCredentialsAsRequirementsForExistingWorkspace(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: runnerTestExistingWorkspace, Namespace: runnerTestNamespace},
		Status:     workspacesv1alpha1.WorkspaceStatus{Conditions: []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, Reason: runnerTestWorkspaceReadyReason}}},
	}
	runner := &Runner{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()}
	_, err := runner.Prepare(context.Background(), RunRequest{Namespace: workspace.Namespace, Workspace: workspace.Name, CredentialRefs: []string{"github"}})
	requirements.EqualError(err, `workspace "existing" does not reference Credential "github"`)
}

func TestRunnerCreatesWorkspaceBeforeGeneratedWorktrees(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: "rc", Namespace: runnerTestNamespace},
		Status:     repositoriesv1alpha1.RepositoryStatus{Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, Reason: runnerTestStorageReadyReason}}},
	}
	conflict := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "generated-conflict", Namespace: runnerTestNamespace}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository, conflict).Build()
	runner := &Runner{Client: kubeClient, NameGenerator: func(string) string { return conflict.Name }}

	_, err := runner.Prepare(context.Background(), RunRequest{Namespace: runnerTestNamespace, Create: true, Remove: true, Image: runnerTestImage, Repositories: []MountRequest{{Name: repository.Name}}})
	requirements.Error(err, "report Workspace create conflict")
	worktrees := new(repositoriesv1alpha1.WorktreeList)
	requirements.NoError(kubeClient.List(context.Background(), worktrees), "list Worktrees after rollback")
	requirements.Empty(worktrees.Items, "do not orphan generated Worktrees")
}

func TestRunnerRollsBackTemporaryTopologyWhenWorktreeCreationFails(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	repositories := []client.Object{
		&repositoriesv1alpha1.Repository{
			ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: runnerTestNamespace},
			Status: repositoriesv1alpha1.RepositoryStatus{Conditions: []metav1.Condition{{
				Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, Reason: runnerTestStorageReadyReason,
			}}},
		},
		&repositoriesv1alpha1.Repository{
			ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: runnerTestNamespace},
			Status: repositoriesv1alpha1.RepositoryStatus{Conditions: []metav1.Condition{{
				Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, Reason: runnerTestStorageReadyReason,
			}}},
		},
	}
	createdWorktrees := 0
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositories...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, kubeClient client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if _, ok := object.(*repositoriesv1alpha1.Worktree); ok {
				createdWorktrees++
				if createdWorktrees == 2 {
					return errors.New("injected Worktree creation failure")
				}
			}
			return kubeClient.Create(ctx, object, options...)
		},
	}).Build()
	runner := &Runner{Client: kubeClient, NameGenerator: func(string) string { return "temporary-rollback" }}

	_, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: runnerTestNamespace, Create: true, Remove: true, Image: runnerTestImage,
		Repositories: []MountRequest{{Name: "first"}, {Name: "second"}},
	})
	requirements.ErrorContains(err, "injected Worktree creation failure", "report the Worktree creation error")
	workspace := new(workspacesv1alpha1.Workspace)
	requirements.Error(kubeClient.Get(context.Background(), client.ObjectKey{Name: "temporary-rollback", Namespace: runnerTestNamespace}, workspace), "delete the partially created Workspace")
	worktrees := new(repositoriesv1alpha1.WorktreeList)
	requirements.NoError(kubeClient.List(context.Background(), worktrees), "list Worktrees after rollback")
	requirements.Empty(worktrees.Items, "delete every Worktree created before the failure")
}

func TestRunnerCreatesRetainedNamedWorkspace(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	runner := &Runner{Client: kubeClient}
	target, err := runner.Prepare(context.Background(), RunRequest{
		Namespace: runnerTestNamespace, Create: true, Name: "retained", Image: runnerTestImage,
	})
	require.NoError(t, err)
	assert.True(t, target.Created)
	assert.Equal(t, "retained", target.Workspace.Name)
	assert.Equal(t, workspacesv1alpha1.WorkspaceRetentionPolicyRetain, target.Workspace.Spec.EffectiveRetentionPolicy())
	assert.False(t, target.Workspace.Spec.IsTemporary())
}

func TestRunnerRejectsPVCConflictBeforeCreatingTopology(t *testing.T) {
	t.Parallel()
	for _, claimName := range []string{"workspace-blocked-home", "worktree-blocked-source"} {
		t.Run(claimName, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
			require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
			repository := &repositoriesv1alpha1.Repository{
				ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: runnerTestNamespace},
				Status:     repositoriesv1alpha1.RepositoryStatus{Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue}}},
			}
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: runnerTestNamespace}}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository, claim).Build()
			_, err := (&Runner{Client: kube}).Prepare(t.Context(), RunRequest{
				Namespace: runnerTestNamespace, Create: true, Name: "blocked", Image: runnerTestImage,
				Repositories: []MountRequest{{Name: repository.Name}},
			})
			require.ErrorContains(t, err, "PVC preflight")
			workspaces := new(workspacesv1alpha1.WorkspaceList)
			require.NoError(t, kube.List(t.Context(), workspaces))
			require.Empty(t, workspaces.Items)
			worktrees := new(repositoriesv1alpha1.WorktreeList)
			require.NoError(t, kube.List(t.Context(), worktrees))
			require.Empty(t, worktrees.Items)
		})
	}
}

func TestRunnerDarwinSkipsHomePVCPreflight(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	denied := errors.New("PVC reads are unavailable")
	kube := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, kube client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, ok := object.(*corev1.PersistentVolumeClaim); ok {
				return denied
			}
			return kube.Get(ctx, key, object, options...)
		},
	}).Build()
	runner := &Runner{Client: kube}
	// Darwin uses a host path, so creation must not depend on PVC read access.
	target, err := runner.Prepare(t.Context(), RunRequest{
		Namespace: runnerTestNamespace, Create: true, Name: "darwin", Image: runnerTestImage, OS: rcplatform.Darwin,
	})
	require.NoError(t, err)
	assert.True(t, target.Created)
	// Generated Worktrees still allocate PVCs, even for a Darwin Workspace.
	err = runner.preflightTarget(t.Context(), target.Workspace, []*repositoriesv1alpha1.Worktree{{ObjectMeta: metav1.ObjectMeta{Name: "mounted", Namespace: runnerTestNamespace}}})
	require.ErrorIs(t, err, denied)
}
