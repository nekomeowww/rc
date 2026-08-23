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
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/cli/rcctl/cluster"
	"github.com/nekomeowww/rc/internal/cli/rcctl/command"
	"github.com/nekomeowww/rc/internal/cli/rcctl/progress"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	repositoryservice "github.com/nekomeowww/rc/internal/repositories"
	workspaceservice "github.com/nekomeowww/rc/internal/workspaces"
)

type createOptions struct {
	environment      string
	image            string
	storageClass     string
	size             string
	defaultCwd       string
	serviceAccount   string
	noServiceAccount bool
	idleTimeout      time.Duration
	wait             bool
	gpu              command.GPUOptions
}

type mountOptions struct {
	workspace string
	path      string
	name      string
	readOnly  bool
	force     bool
}

func Register(root *cobra.Command, kubeconfigFlags *kubeconfig.Flags) {
	root.AddCommand(NewCommand(kubeconfigFlags))
}

func NewCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	root := &cobra.Command{Use: "workspace", Aliases: []string{"workspaces"}, Short: "Manage persistent development machines", GroupID: command.WorkspacesGroup}
	root.AddCommand(newCreateCommand(kubeconfigFlags), newMountCommand(kubeconfigFlags), newUnmountCommand(kubeconfigFlags), newStateCommand(kubeconfigFlags, true), newStateCommand(kubeconfigFlags, false), newDeleteCommand(kubeconfigFlags), newPortForwardCommand(kubeconfigFlags), newDefaultCommand(kubeconfigFlags), newListCommand(kubeconfigFlags))

	return root
}

func newCreateCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := new(createOptions)
	cmd := &cobra.Command{
		Use: "create NAME", Short: "Create a persistent Workspace", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resources, err := options.gpu.ResourceRequirements(cmd.Flags().Changed("gpu"), cmd.Flags().Changed("gpu-vram"))
			if err != nil {
				return err
			}
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			workspace := &workspacesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{Name: args[0], Namespace: namespace},
				Spec: workspacesv1alpha1.WorkspaceSpec{
					DesiredState: workspacesv1alpha1.WorkspaceDesiredStateRunning,
					Image:        options.image, DefaultWorkingDirectory: options.defaultCwd,
					ServiceAccountName: options.serviceAccount,
					Resources:          resources,
				},
			}
			if options.environment != "" {
				environment := new(workspacesv1alpha1.WorkspaceEnvironment)
				if err := clusterClient.Kube.Get(cmd.Context(), client.ObjectKey{Name: options.environment, Namespace: namespace}, environment); err != nil {
					return fmt.Errorf("get WorkspaceEnvironment %q: %w", options.environment, err)
				}
				if !meta.IsStatusConditionTrue(environment.Status.Conditions, workspacesv1alpha1.WorkspaceEnvironmentConditionReady) {
					return fmt.Errorf("workspace environment %q is not Ready", options.environment)
				}
				workspace.Spec.EnvironmentRef = &workspacesv1alpha1.LocalReference{Name: options.environment}
			} else {
				size, err := resource.ParseQuantity(options.size)
				if err != nil || size.Sign() <= 0 {
					return fmt.Errorf("parse --size: value must be a positive Kubernetes quantity")
				}
				workspace.Spec.Storage = &workspacesv1alpha1.PersistentStorageSpec{StorageClassName: options.storageClass, Size: size}
			}
			if options.noServiceAccount && options.serviceAccount != "" {
				return fmt.Errorf("--no-service-account and --service-account are mutually exclusive")
			}
			if options.noServiceAccount {
				workspace.Spec.AutomountServiceAccountToken = boolPointer(false)
			}
			if options.idleTimeout > 0 {
				workspace.Spec.IdleTimeout = &metav1.Duration{Duration: options.idleTimeout}
			}
			if err := clusterClient.Kube.Create(cmd.Context(), workspace); err != nil {
				return fmt.Errorf("create Workspace: %w", err)
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "workspace.workspaces.rc.ayaka.io/%s created\n", workspace.Name); err != nil {
				return err
			}
			if options.wait {
				indicator := progress.Start(cmd.ErrOrStderr(), "preparing Workspace...")
				defer indicator.Stop()
				return waitWorkspaceReady(cmd.Context(), clusterClient.Kube, workspace)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&options.environment, "environment", "", "WorkspaceEnvironment to clone")
	cmd.Flags().StringVar(&options.image, "image", "", "Runner image for a blank Workspace")
	cmd.Flags().StringVar(&options.storageClass, "storage-class", "", "StorageClass for a blank Workspace")
	cmd.Flags().StringVar(&options.size, "size", "20Gi", "Home volume size for a blank Workspace")
	cmd.Flags().StringVar(&options.defaultCwd, "cwd", "", "Default process working directory")
	cmd.Flags().StringVar(&options.serviceAccount, "service-account", "", "Same-namespace ServiceAccount")
	cmd.Flags().BoolVar(&options.noServiceAccount, "no-service-account", false, "Disable ServiceAccount token mounting")
	cmd.Flags().DurationVar(&options.idleTimeout, "idle-timeout", 0, "Suspend an idle named Workspace; zero disables")
	cmd.Flags().BoolVar(&options.wait, "wait", true, "Wait for the Workspace runtime")
	options.gpu.AddFlags(cmd.Flags())

	return cmd
}

func newMountCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	root := &cobra.Command{Use: "mount", Short: "Add a code mount, replacing an idle runtime Pod"}
	repositoryOptions := new(mountOptions)
	repository := &cobra.Command{
		Use: "repo REPOSITORY", Short: "Create and mount a writable Worktree, or explicitly mount Repository read-only", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return mountRepository(cmd, kubeconfigFlags, args[0], *repositoryOptions)
		},
	}
	addMountFlags(repository, repositoryOptions)
	repository.Flags().BoolVar(&repositoryOptions.readOnly, "read-only", false, "Mount the Repository parent itself without creating a Worktree")
	worktreeOptions := new(mountOptions)
	worktree := &cobra.Command{
		Use: "worktree WORKTREE", Short: "Mount an existing Worktree", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return mountWorktree(cmd, kubeconfigFlags, args[0], *worktreeOptions)
		},
	}
	addMountFlags(worktree, worktreeOptions)
	worktree.Flags().BoolVar(&worktreeOptions.readOnly, "read-only", false, "Mount the Worktree read-only")
	root.AddCommand(repository, worktree)

	return root
}

func addMountFlags(cmd *cobra.Command, options *mountOptions) {
	cmd.Flags().StringVar(&options.workspace, "workspace", "", "Workspace name; defaults from XDG config")
	cmd.Flags().StringVar(&options.path, "path", "", "Path below /workspace; defaults to mount name")
	cmd.Flags().StringVar(&options.name, "name", "", "Mount name; defaults to source name")
	cmd.Flags().BoolVar(&options.force, "force", false, "Stop active processes before replacing topology")
}

func mountRepository(cmd *cobra.Command, kubeconfigFlags *kubeconfig.Flags, selector string, options mountOptions) error {
	config, namespace, contextName, err := kubeconfigFlags.ResolveWithIdentity()
	if err != nil {
		return err
	}
	clusterClient, err := cluster.New(config)
	if err != nil {
		return err
	}
	workspace, err := selectedWorkspace(cmd.Context(), clusterClient.Kube, namespace, contextName, options.workspace)
	if err != nil {
		return err
	}
	if err := stopForTopologyChange(cmd.Context(), clusterClient, workspace, options.force); err != nil {
		return err
	}
	repository, err := repositoryservice.ResolveRepository(cmd.Context(), clusterClient.Kube, namespace, selector)
	if err != nil {
		return err
	}
	mountName := options.name
	if mountName == "" {
		mountName = repository.Name
	}
	mountPath := options.path
	if mountPath == "" {
		mountPath = mountName
	}
	mount := workspacesv1alpha1.WorkspaceMount{Name: mountName, Path: mountPath}
	if options.readOnly {
		mount.RepositoryRef = &workspacesv1alpha1.LocalReference{Name: repository.Name}
		mount.ReadOnly = true
	} else {
		worktreeName := boundedName(workspace.Name + "-" + mountName)
		worktree := &repositoriesv1alpha1.Worktree{
			ObjectMeta: metav1.ObjectMeta{Name: worktreeName, Namespace: namespace, Labels: map[string]string{workspaceservice.GeneratedWorkspaceLabel: workspace.Name}},
			Spec: repositoriesv1alpha1.WorktreeSpec{
				RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repository.Name}, Branch: "rc/" + workspace.Name + "/" + mountName,
			},
		}
		if err := clusterClient.Kube.Create(cmd.Context(), worktree); err != nil {
			return fmt.Errorf("create mounted Worktree: %w", err)
		}
		mount.WorktreeRef = &workspacesv1alpha1.LocalReference{Name: worktree.Name}
	}

	if err := appendMount(cmd.Context(), clusterClient.Kube, workspace, mount); err != nil {
		if mount.WorktreeRef != nil {
			worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: mount.WorktreeRef.Name, Namespace: namespace}}
			if cleanupErr := clusterClient.Kube.Delete(cmd.Context(), worktree); cleanupErr != nil {
				return errors.Join(err, fmt.Errorf("delete generated Worktree after mount failure: %w", cleanupErr))
			}
		}
		return err
	}
	return nil
}

func mountWorktree(cmd *cobra.Command, kubeconfigFlags *kubeconfig.Flags, selector string, options mountOptions) error {
	config, namespace, contextName, err := kubeconfigFlags.ResolveWithIdentity()
	if err != nil {
		return err
	}
	clusterClient, err := cluster.New(config)
	if err != nil {
		return err
	}
	workspace, err := selectedWorkspace(cmd.Context(), clusterClient.Kube, namespace, contextName, options.workspace)
	if err != nil {
		return err
	}
	if err := stopForTopologyChange(cmd.Context(), clusterClient, workspace, options.force); err != nil {
		return err
	}
	name := selector
	repositorySelector := ""
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		repositorySelector = name[:slash]
		name = name[slash+1:]
	}
	worktree := new(repositoriesv1alpha1.Worktree)
	if err := clusterClient.Kube.Get(cmd.Context(), client.ObjectKey{Name: name, Namespace: namespace}, worktree); err != nil {
		return fmt.Errorf("get Worktree: %w", err)
	}
	if repositorySelector != "" {
		repository, err := repositoryservice.ResolveRepository(cmd.Context(), clusterClient.Kube, namespace, repositorySelector)
		if err != nil {
			return err
		}
		if worktree.Spec.RepositoryRef.Name != repository.Name {
			return fmt.Errorf("worktree %q belongs to Repository %q, not %q", worktree.Name, worktree.Spec.RepositoryRef.Name, repository.Name)
		}
	}
	mountName := options.name
	if mountName == "" {
		mountName = worktree.Name
	}
	mountPath := options.path
	if mountPath == "" {
		mountPath = mountName
	}
	mount := workspacesv1alpha1.WorkspaceMount{
		Name: mountName, Path: mountPath, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}, ReadOnly: options.readOnly,
	}

	return appendMount(cmd.Context(), clusterClient.Kube, workspace, mount)
}

func appendMount(ctx context.Context, kubeClient client.Client, workspace *workspacesv1alpha1.Workspace, mount workspacesv1alpha1.WorkspaceMount) error {
	current := new(workspacesv1alpha1.Workspace)
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		return fmt.Errorf("re-fetch Workspace before mount: %w", err)
	}
	for _, existing := range current.Spec.Mounts {
		if existing.Name == mount.Name || existing.Path == mount.Path {
			return fmt.Errorf("workspace mount name or path %q already exists", mount.Name)
		}
	}
	current.Spec.Mounts = append(current.Spec.Mounts, mount)
	if err := kubeClient.Update(ctx, current); err != nil {
		return fmt.Errorf("update Workspace mounts: %w", err)
	}

	return nil
}

func newUnmountCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use: "unmount WORKSPACE MOUNT", Short: "Remove a code mount, replacing an idle runtime Pod", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			workspace := new(workspacesv1alpha1.Workspace)
			if err := clusterClient.Kube.Get(cmd.Context(), client.ObjectKey{Name: args[0], Namespace: namespace}, workspace); err != nil {
				return err
			}
			if err := stopForTopologyChange(cmd.Context(), clusterClient, workspace, force); err != nil {
				return err
			}
			current := new(workspacesv1alpha1.Workspace)
			if err := clusterClient.Kube.Get(cmd.Context(), client.ObjectKeyFromObject(workspace), current); err != nil {
				return err
			}
			mounts := current.Spec.Mounts[:0]
			found := false
			for _, mount := range current.Spec.Mounts {
				if mount.Name == args[1] {
					found = true
					continue
				}
				mounts = append(mounts, mount)
			}
			if !found {
				return fmt.Errorf("workspace %q has no mount %q", current.Name, args[1])
			}
			current.Spec.Mounts = mounts
			return clusterClient.Kube.Update(cmd.Context(), current)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Stop active processes before replacing topology")

	return cmd
}

func newStateCommand(kubeconfigFlags *kubeconfig.Flags, running bool) *cobra.Command {
	var force bool
	verb := "stop"
	short := "Suspend a Workspace runtime while retaining storage"
	if running {
		verb = "start"
		short = "Start a suspended Workspace runtime"
	}
	cmd := &cobra.Command{
		Use: verb + " NAME", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			workspace := new(workspacesv1alpha1.Workspace)
			key := client.ObjectKey{Name: args[0], Namespace: namespace}
			if err := clusterClient.Kube.Get(cmd.Context(), key, workspace); err != nil {
				return err
			}
			if !running {
				if err := stopForTopologyChange(cmd.Context(), clusterClient, workspace, force); err != nil {
					return err
				}
				if err := clusterClient.Kube.Get(cmd.Context(), key, workspace); err != nil {
					return err
				}
				workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
			} else {
				workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateRunning
			}
			return clusterClient.Kube.Update(cmd.Context(), workspace)
		},
	}
	if !running {
		cmd.Flags().BoolVar(&force, "force", false, "Stop active processes before suspension")
	}

	return cmd
}

func newDeleteCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	var force bool
	var cascade bool
	cmd := &cobra.Command{
		Use: "delete NAME", Short: "Delete a Workspace and its owned runtime state", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			workspace := new(workspacesv1alpha1.Workspace)
			if err := clusterClient.Kube.Get(cmd.Context(), client.ObjectKey{Name: args[0], Namespace: namespace}, workspace); err != nil {
				return err
			}
			if err := stopForTopologyChange(cmd.Context(), clusterClient, workspace, force); err != nil {
				return err
			}
			createdWorktrees := new(repositoriesv1alpha1.WorktreeList)
			if cascade {
				if err := clusterClient.Kube.List(cmd.Context(), createdWorktrees, client.InNamespace(namespace), client.MatchingLabels{workspaceservice.GeneratedWorkspaceLabel: workspace.Name}); err != nil {
					return err
				}
				for _, worktree := range createdWorktrees.Items {
					if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "cascade delete worktree/%s\n", worktree.Name); err != nil {
						return err
					}
				}
			}
			if err := clusterClient.Kube.Delete(cmd.Context(), workspace); err != nil {
				return err
			}
			for index := range createdWorktrees.Items {
				if err := clusterClient.Kube.Delete(cmd.Context(), &createdWorktrees.Items[index]); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Stop active processes before deletion")
	cmd.Flags().BoolVar(&cascade, "cascade-created-worktrees", false, "Preview and delete Worktrees created for this Workspace")

	return cmd
}

func newPortForwardCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "port-forward NAME LOCAL[:REMOTE]", Short: "Forward a local port to the Workspace Pod", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			workspace := new(workspacesv1alpha1.Workspace)
			if err := clusterClient.Kube.Get(cmd.Context(), client.ObjectKey{Name: args[0], Namespace: namespace}, workspace); err != nil {
				return err
			}
			if workspace.Status.RuntimePodName == "" {
				return fmt.Errorf("workspace %q has no running Pod", workspace.Name)
			}
			roundTripper, upgrader, err := spdy.RoundTripperFor(config)
			if err != nil {
				return err
			}
			serverURL, err := url.Parse(config.Host)
			if err != nil {
				return err
			}
			serverURL.Path = fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", namespace, workspace.Status.RuntimePodName)
			dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, serverURL)
			port := args[1]
			if !strings.Contains(port, ":") {
				port += ":" + port
			}
			stop := make(chan struct{})
			ready := make(chan struct{})
			go func() { <-cmd.Context().Done(); close(stop) }()
			forwarder, err := portforward.New(dialer, []string{port}, stop, ready, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return forwarder.ForwardPorts()
		},
	}
}

func newDefaultCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "default NAME", Short: "Set the XDG default Workspace for the current context and namespace", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			_, namespace, contextName, err := kubeconfigFlags.ResolveWithIdentity()
			if err != nil {
				return err
			}
			path, err := workspaceservice.DefaultConfigPath()
			if err != nil {
				return err
			}
			store := workspaceservice.DefaultStore{Path: path}
			defaults, err := store.Get(contextName, namespace)
			if err != nil {
				return err
			}
			defaults.Workspace = args[0]
			return store.Set(contextName, namespace, defaults)
		},
	}
}

func newListCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List Workspaces in the current namespace", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			list := new(workspacesv1alpha1.WorkspaceList)
			if err := clusterClient.Kube.List(cmd.Context(), list, client.InNamespace(namespace)); err != nil {
				return err
			}
			for _, workspace := range list.Items {
				ready := meta.IsStatusConditionTrue(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%t\t%s\n", workspace.Name, workspace.Spec.DesiredState, ready, workspace.Status.RuntimePodName); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func selectedWorkspace(ctx context.Context, kubeClient client.Client, namespace string, contextName string, explicit string) (*workspacesv1alpha1.Workspace, error) {
	name := explicit
	if name == "" {
		path, err := workspaceservice.DefaultConfigPath()
		if err != nil {
			return nil, err
		}
		defaults, err := (workspaceservice.DefaultStore{Path: path}).Get(contextName, namespace)
		if err != nil {
			return nil, err
		}
		name = defaults.Workspace
	}
	if name == "" {
		return nil, fmt.Errorf("select a Workspace with --workspace or rcctl workspace default")
	}
	workspace := new(workspacesv1alpha1.Workspace)
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, workspace); err != nil {
		return nil, fmt.Errorf("get Workspace %q: %w", name, err)
	}

	return workspace, nil
}

func stopForTopologyChange(ctx context.Context, clusterClient *cluster.Client, workspace *workspacesv1alpha1.Workspace, force bool) error {
	processes := new(workspacesv1alpha1.AgentProcessList)
	if err := clusterClient.Kube.List(ctx, processes, client.InNamespace(workspace.Namespace)); err != nil {
		return err
	}
	processClient := &workspaceservice.ProcessClient{Kube: clusterClient.Kube, Runtime: clusterClient.Processes, Config: clusterClient.Config}
	for index := range processes.Items {
		process := &processes.Items[index]
		if process.Spec.TargetRef.Kind != workspacesv1alpha1.AgentProcessTargetWorkspace || process.Spec.TargetRef.Name != workspace.Name || terminal(process.Status.Phase) {
			continue
		}
		if !force {
			return fmt.Errorf("workspace %q has active AgentProcess %q; use --force", workspace.Name, process.Name)
		}
		if err := processClient.Stop(ctx, process); err != nil {
			return err
		}
		if _, err := processClient.WaitUntilTerminal(ctx, process); err != nil {
			return err
		}
	}

	return nil
}

func waitWorkspaceReady(ctx context.Context, kubeClient client.Client, workspace *workspacesv1alpha1.Workspace) error {
	return wait.PollUntilContextCancel(ctx, 300*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current := new(workspacesv1alpha1.Workspace)
		if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(current.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady), nil
	})
}

func terminal(phase workspacesv1alpha1.AgentProcessPhase) bool {
	switch phase {
	case workspacesv1alpha1.AgentProcessPhaseSucceeded, workspacesv1alpha1.AgentProcessPhaseFailed,
		workspacesv1alpha1.AgentProcessPhaseStopped, workspacesv1alpha1.AgentProcessPhaseLost:
		return true
	default:
		return false
	}
}

func boundedName(name string) string {
	if len(name) <= 63 {
		return strings.Trim(name, "-")
	}
	return strings.TrimRight(name[:63], "-")
}

func boolPointer(value bool) *bool {
	return &value
}
