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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const GeneratedWorkspaceLabel = "workspaces.rc.ayaka.io/generated-for"

type MountRequest struct {
	Name      string
	MountName string
	Path      string
	ReadOnly  bool
}

type RunRequest struct {
	Namespace                    string
	Workspace                    string
	DefaultWorkspace             string
	Temporary                    bool
	Environment                  string
	DefaultEnvironment           string
	Repositories                 []MountRequest
	Worktrees                    []MountRequest
	AgentCredentialRefs          []string
	CredentialRefs               []string
	Storage                      *workspacesv1alpha1.PersistentStorageSpec
	Image                        string
	Resources                    corev1.ResourceRequirements
	ServiceAccountName           string
	AutomountServiceAccountToken *bool
	NamePrefix                   string
}

// UsesGeneratedWorkspace reports whether a request needs an independent
// Workspace instead of an explicitly or implicitly selected existing one.
func (request RunRequest) UsesGeneratedWorkspace() bool {
	return request.Workspace == "" && (request.Temporary || request.DefaultWorkspace == "" ||
		(len(request.Repositories) == 0 && len(request.Worktrees) == 0))
}

type RunTarget struct {
	Workspace *workspacesv1alpha1.Workspace
	Created   bool
}

type Runner struct {
	Client        client.Client
	NameGenerator func(string) string
}

func (runner *Runner) Prepare(ctx context.Context, request RunRequest) (RunTarget, error) {
	workspaceName := request.Workspace
	if workspaceName == "" && !request.UsesGeneratedWorkspace() {
		workspaceName = request.DefaultWorkspace
	}
	if workspaceName != "" {
		workspace := new(workspacesv1alpha1.Workspace)
		key := client.ObjectKey{Name: workspaceName, Namespace: request.Namespace}
		if err := runner.Client.Get(ctx, key, workspace); err != nil {
			if apierrors.IsNotFound(err) {
				return RunTarget{}, fmt.Errorf("workspace %q does not exist", workspaceName)
			}
			return RunTarget{}, fmt.Errorf("get Workspace %q: %w", workspaceName, err)
		}
		if !meta.IsStatusConditionTrue(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady) {
			return RunTarget{}, fmt.Errorf("workspace %q is not Ready", workspaceName)
		}
		if err := runner.validateExistingTarget(ctx, workspace, request); err != nil {
			return RunTarget{}, err
		}

		return RunTarget{Workspace: workspace}, nil
	}

	return runner.createGeneratedTarget(ctx, request)
}

func (runner *Runner) validateExistingTarget(ctx context.Context, workspace *workspacesv1alpha1.Workspace, request RunRequest) error {
	if request.Environment != "" {
		if workspace.Spec.EnvironmentRef == nil || workspace.Spec.EnvironmentRef.Name != request.Environment {
			return fmt.Errorf("workspace %q does not use WorkspaceEnvironment %q", workspace.Name, request.Environment)
		}
	}
	for _, requirement := range request.Repositories {
		found := false
		for _, mount := range workspace.Spec.Mounts {
			if mount.RepositoryRef != nil && mount.RepositoryRef.Name == requirement.Name {
				found = true
				break
			}
			if mount.WorktreeRef == nil {
				continue
			}
			worktree := new(repositoriesv1alpha1.Worktree)
			key := client.ObjectKey{Name: mount.WorktreeRef.Name, Namespace: workspace.Namespace}
			if err := runner.Client.Get(ctx, key, worktree); err != nil {
				return fmt.Errorf("get Workspace mounted Worktree %q: %w", mount.WorktreeRef.Name, err)
			}
			if worktree.Spec.RepositoryRef.Name == requirement.Name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("workspace %q does not have a mount derived from Repository %q", workspace.Name, requirement.Name)
		}
	}
	for _, requirement := range request.Worktrees {
		found := false
		for _, mount := range workspace.Spec.Mounts {
			if mount.WorktreeRef != nil && mount.WorktreeRef.Name == requirement.Name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("workspace %q does not mount Worktree %q", workspace.Name, requirement.Name)
		}
	}
	for _, requirement := range request.AgentCredentialRefs {
		if !containsLocalReference(workspace.Spec.AgentCredentialRefs, requirement) {
			return fmt.Errorf("workspace %q does not reference AgentCredential %q", workspace.Name, requirement)
		}
	}
	for _, requirement := range request.CredentialRefs {
		if !containsLocalReference(workspace.Spec.CredentialRefs, requirement) {
			return fmt.Errorf("workspace %q does not reference Credential %q", workspace.Name, requirement)
		}
	}

	return nil
}

func containsLocalReference(references []workspacesv1alpha1.LocalReference, name string) bool {
	for _, reference := range references {
		if reference.Name == name {
			return true
		}
	}

	return false
}

func (runner *Runner) createGeneratedTarget(ctx context.Context, request RunRequest) (target RunTarget, returnedErr error) {
	prefix := request.NamePrefix
	if prefix == "" {
		prefix = "workspace"
	}
	nameGenerator := runner.NameGenerator
	if nameGenerator == nil {
		nameGenerator = GenerateSortableName
	}
	workspaceName := nameGenerator(prefix)
	environmentName := request.Environment
	if environmentName == "" {
		environmentName = request.DefaultEnvironment
	}
	if environmentName == "" && request.Storage == nil {
		request.Storage = &workspacesv1alpha1.PersistentStorageSpec{Size: resource.MustParse("20Gi")}
	}
	if environmentName != "" {
		environment := new(workspacesv1alpha1.WorkspaceEnvironment)
		key := client.ObjectKey{Name: environmentName, Namespace: request.Namespace}
		if err := runner.Client.Get(ctx, key, environment); err != nil {
			return RunTarget{}, fmt.Errorf("get WorkspaceEnvironment %q: %w", environmentName, err)
		}
		if !meta.IsStatusConditionTrue(environment.Status.Conditions, workspacesv1alpha1.WorkspaceEnvironmentConditionReady) {
			return RunTarget{}, fmt.Errorf("workspace environment %q is not Ready", environmentName)
		}
	}

	mounts := make([]workspacesv1alpha1.WorkspaceMount, 0, len(request.Repositories)+len(request.Worktrees))
	mountNames := make(map[string]struct{})
	createdWorktrees := make([]*repositoriesv1alpha1.Worktree, 0, len(request.Repositories))
	defer func() {
		if returnedErr == nil {
			return
		}
		cleanupContext := context.WithoutCancel(ctx)
		for _, worktree := range createdWorktrees {
			if err := runner.Client.Delete(cleanupContext, worktree); err != nil && !apierrors.IsNotFound(err) {
				returnedErr = errors.Join(returnedErr, fmt.Errorf("delete generated Worktree %q after Workspace creation failed: %w", worktree.Name, err))
			}
		}
	}()
	for _, source := range request.Repositories {
		repository := new(repositoriesv1alpha1.Repository)
		key := client.ObjectKey{Name: source.Name, Namespace: request.Namespace}
		if err := runner.Client.Get(ctx, key, repository); err != nil {
			return RunTarget{}, fmt.Errorf("get Repository %q: %w", source.Name, err)
		}
		if !meta.IsStatusConditionTrue(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady) {
			return RunTarget{}, fmt.Errorf("repository %q is not Ready", source.Name)
		}
		mountName, mountPath, err := normalizedMount(source)
		if err != nil {
			return RunTarget{}, err
		}
		if _, exists := mountNames[mountName]; exists {
			return RunTarget{}, fmt.Errorf("mount name %q is selected more than once", mountName)
		}
		mountNames[mountName] = struct{}{}
		worktreeName := boundedDNSName(workspaceName + "-" + mountName)
		worktree := &repositoriesv1alpha1.Worktree{
			ObjectMeta: metav1.ObjectMeta{
				Name: worktreeName, Namespace: request.Namespace,
				Labels: map[string]string{GeneratedWorkspaceLabel: workspaceName},
			},
			Spec: repositoriesv1alpha1.WorktreeSpec{
				RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repository.Name},
				Branch:        "rc/" + workspaceName + "/" + mountName,
			},
		}
		if err := runner.Client.Create(ctx, worktree); err != nil {
			return RunTarget{}, fmt.Errorf("create generated Worktree %q: %w", worktree.Name, err)
		}
		createdWorktrees = append(createdWorktrees, worktree)
		mounts = append(mounts, workspacesv1alpha1.WorkspaceMount{
			Name: mountName, Path: mountPath, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name},
		})
	}
	for _, source := range request.Worktrees {
		worktree := new(repositoriesv1alpha1.Worktree)
		key := client.ObjectKey{Name: source.Name, Namespace: request.Namespace}
		if err := runner.Client.Get(ctx, key, worktree); err != nil {
			return RunTarget{}, fmt.Errorf("get Worktree %q: %w", source.Name, err)
		}
		if !meta.IsStatusConditionTrue(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady) {
			return RunTarget{}, fmt.Errorf("worktree %q is not Ready", source.Name)
		}
		mountName, mountPath, err := normalizedMount(source)
		if err != nil {
			return RunTarget{}, err
		}
		if _, exists := mountNames[mountName]; exists {
			return RunTarget{}, fmt.Errorf("mount name %q is selected more than once", mountName)
		}
		mountNames[mountName] = struct{}{}
		mounts = append(mounts, workspacesv1alpha1.WorkspaceMount{
			Name: mountName, Path: mountPath, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}, ReadOnly: source.ReadOnly,
		})
	}

	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceName, Namespace: request.Namespace},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState: workspacesv1alpha1.WorkspaceDesiredStateRunning, Generated: true,
			Image: request.Image, Storage: request.Storage, Mounts: mounts, Resources: request.Resources,
			ServiceAccountName: request.ServiceAccountName, AutomountServiceAccountToken: request.AutomountServiceAccountToken,
		},
	}
	if environmentName != "" {
		workspace.Spec.EnvironmentRef = &workspacesv1alpha1.LocalReference{Name: environmentName}
	}
	for _, name := range request.AgentCredentialRefs {
		workspace.Spec.AgentCredentialRefs = append(workspace.Spec.AgentCredentialRefs, workspacesv1alpha1.LocalReference{Name: name})
	}
	for _, name := range request.CredentialRefs {
		workspace.Spec.CredentialRefs = append(workspace.Spec.CredentialRefs, workspacesv1alpha1.LocalReference{Name: name})
	}
	if err := runner.Client.Create(ctx, workspace); err != nil {
		return RunTarget{}, fmt.Errorf("create generated Workspace %q: %w", workspace.Name, err)
	}

	return RunTarget{Workspace: workspace, Created: true}, nil
}

func normalizedMount(source MountRequest) (string, string, error) {
	mountName := source.MountName
	if mountName == "" {
		mountName = source.Name
		if slash := strings.LastIndex(mountName, "/"); slash >= 0 {
			mountName = mountName[slash+1:]
		}
	}
	mountPath := source.Path
	if mountPath == "" {
		mountPath = mountName
	}
	if mountName == "" || mountPath == "" {
		return "", "", fmt.Errorf("mount name and path cannot be empty")
	}

	return mountName, mountPath, nil
}

func GenerateSortableName(prefix string) string {
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		panic(fmt.Sprintf("read cryptographic randomness: %v", err))
	}
	timestamp := strconv.FormatInt(time.Now().UTC().UnixMilli(), 36)
	return boundedDNSName(strings.ToLower(prefix) + "-" + timestamp + hex.EncodeToString(random))
}

func boundedDNSName(name string) string {
	name = strings.Trim(name, "-")
	if len(name) <= 63 {
		return name
	}

	return strings.TrimRight(name[:63], "-")
}
