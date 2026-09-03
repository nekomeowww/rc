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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/cli/rcctl/cluster"
	"github.com/nekomeowww/rc/internal/cli/rcctl/command"
	"github.com/nekomeowww/rc/internal/cli/rcctl/progress"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	repositoryservice "github.com/nekomeowww/rc/internal/repositories"
	workspaceservice "github.com/nekomeowww/rc/internal/workspaces"
	clioutput "github.com/nekomeowww/rc/pkg/output"
)

type createOptions struct {
	environment      string
	image            string
	storageClass     string
	size             string
	agentCredentials []string
	credentials      []string
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

type listOptions struct {
	output clioutput.Options
}

func Register(root *cobra.Command, kubeconfigFlags *kubeconfig.Flags) {
	root.AddCommand(NewCommand(kubeconfigFlags))
}

func NewCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	root := &cobra.Command{Use: "workspace", Aliases: []string{"workspaces"}, Short: "Manage persistent development machines", GroupID: command.WorkspacesGroup}
	root.AddCommand(newCreateCommand(kubeconfigFlags), newMountCommand(kubeconfigFlags), newUnmountCommand(kubeconfigFlags), newStateCommand(kubeconfigFlags, true), newStateCommand(kubeconfigFlags, false), newDeleteCommand(kubeconfigFlags), newPortForwardCommand(kubeconfigFlags), newDefaultCommand(kubeconfigFlags), newListCommand(kubeconfigFlags), newGetCommand(kubeconfigFlags))

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
			if err := setWorkspaceCredentialReferences(cmd.Context(), clusterClient.Kube, workspace, options.agentCredentials, options.credentials); err != nil {
				return err
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
	cmd.Flags().StringArrayVar(&options.agentCredentials, "agent-credential", nil, "Ordered AgentCredential names available to Agent Processes; repeat")
	cmd.Flags().StringArrayVar(&options.credentials, "credential", nil, "Credential names available to Agent Processes; repeat")
	cmd.Flags().StringVar(&options.defaultCwd, "cwd", "", "Default process working directory")
	cmd.Flags().StringVar(&options.serviceAccount, "service-account", "", "Same-namespace ServiceAccount")
	cmd.Flags().BoolVar(&options.noServiceAccount, "no-service-account", false, "Disable ServiceAccount token mounting")
	cmd.Flags().DurationVar(&options.idleTimeout, "idle-timeout", 0, "Suspend an idle named Workspace; zero disables")
	cmd.Flags().BoolVar(&options.wait, "wait", true, "Wait for the Workspace runtime")
	options.gpu.AddFlags(cmd.Flags())

	return cmd
}

func setWorkspaceCredentialReferences(
	ctx context.Context,
	kubeClient client.Client,
	workspace *workspacesv1alpha1.Workspace,
	agentCredentialNames []string,
	credentialNames []string,
) error {
	agentCredentialRefs := make([]workspacesv1alpha1.LocalReference, 0, len(agentCredentialNames))
	for _, name := range agentCredentialNames {
		credential := new(configsv1alpha1.AgentCredential)
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: workspace.Namespace}, credential); err != nil {
			return fmt.Errorf("get AgentCredential %q: %w", name, err)
		}
		agentCredentialRefs = append(agentCredentialRefs, workspacesv1alpha1.LocalReference{Name: name})
	}
	credentialRefs := make([]workspacesv1alpha1.LocalReference, 0, len(credentialNames))
	for _, name := range credentialNames {
		credential := new(configsv1alpha1.Credential)
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: workspace.Namespace}, credential); err != nil {
			return fmt.Errorf("get Credential %q: %w", name, err)
		}
		credentialRefs = append(credentialRefs, workspacesv1alpha1.LocalReference{Name: name})
	}
	workspace.Spec.AgentCredentialRefs = agentCredentialRefs
	workspace.Spec.CredentialRefs = credentialRefs

	return nil
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
	options := new(listOptions)
	cmd := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List Workspaces in the current namespace", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := options.output.Validate(true); err != nil {
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
			list := new(workspacesv1alpha1.WorkspaceList)
			if err := clusterClient.Kube.List(cmd.Context(), list, client.InNamespace(namespace)); err != nil {
				return err
			}
			list.Items = workspaceListItems(list.Items)
			return options.output.PrintList(
				cmd.OutOrStdout(), list, clusterClient.Kube.Scheme(), workspaceListTable(list.Items, time.Now()),
			)
		},
	}
	options.output.AddFlags(cmd, true)

	return cmd
}

func newGetCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := new(clioutput.Options)
	cmd := &cobra.Command{
		Use: "get NAME", Short: "Show a Workspace", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := options.Validate(false); err != nil {
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
			workspace := new(workspacesv1alpha1.Workspace)
			if err := clusterClient.Kube.Get(cmd.Context(), client.ObjectKey{Namespace: namespace, Name: args[0]}, workspace); err != nil {
				return fmt.Errorf("get Workspace %q: %w", args[0], err)
			}
			return options.PrintDetails(cmd.OutOrStdout(), workspace, clusterClient.Kube.Scheme(), workspaceDetailFields(workspace))
		},
	}
	options.AddFlags(cmd, false)

	return cmd
}

func workspaceListItems(workspaces []workspacesv1alpha1.Workspace) []workspacesv1alpha1.Workspace {
	items := slices.Clone(workspaces)
	slices.SortFunc(items, func(left workspacesv1alpha1.Workspace, right workspacesv1alpha1.Workspace) int {
		return strings.Compare(left.Name, right.Name)
	})

	return items
}

func workspaceListTable(workspaces []workspacesv1alpha1.Workspace, now time.Time) clioutput.Table {
	rows := make([][]any, 0, len(workspaces))
	for _, workspace := range workspaces {
		ready := meta.IsStatusConditionTrue(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
		environment := "-"
		if workspace.Spec.EnvironmentRef != nil {
			environment = clioutput.ValueOrDash(workspace.Spec.EnvironmentRef.Name)
		}
		age := "<unknown>"
		if !workspace.CreationTimestamp.IsZero() {
			age = duration.HumanDuration(now.Sub(workspace.CreationTimestamp.Time))
		}
		rows = append(rows, []any{
			workspace.Name, clioutput.ValueOrDash(string(workspace.Spec.DesiredState)), ready, workspace.Spec.Generated,
			environment, clioutput.ValueOrDash(workspace.Status.RuntimeImage), clioutput.ValueOrDash(workspace.Status.RuntimePodName), age,
		})
	}

	return clioutput.Table{
		Columns: []clioutput.Column{
			{Name: "NAME", MaxWidth: 32}, {Name: "STATE"}, {Name: "READY"}, {Name: "GENERATED", Wide: true},
			{Name: "ENVIRONMENT", MaxWidth: 32, Wide: true}, {Name: "IMAGE", MaxWidth: 40, Flexible: true, Wide: true},
			{Name: "RUNTIME POD", MaxWidth: 40, Flexible: true}, {Name: "AGE"},
		},
		Rows: rows,
	}
}

func workspaceDetailFields(workspace *workspacesv1alpha1.Workspace) []clioutput.Field {
	environment := "-"
	if workspace.Spec.EnvironmentRef != nil {
		environment = clioutput.ValueOrDash(workspace.Spec.EnvironmentRef.Name)
	}
	storage := "-"
	if workspace.Spec.Storage != nil {
		storage = workspaceStorageSummary(*workspace.Spec.Storage)
	}
	idleTimeout := "-"
	if workspace.Spec.IdleTimeout != nil {
		idleTimeout = workspace.Spec.IdleTimeout.Duration.String()
	}
	automountToken := "default"
	if workspace.Spec.AutomountServiceAccountToken != nil {
		automountToken = strconv.FormatBool(*workspace.Spec.AutomountServiceAccountToken)
	}
	runtimeClass := "-"
	if workspace.Spec.RuntimeClassName != nil {
		runtimeClass = clioutput.ValueOrDash(*workspace.Spec.RuntimeClassName)
	}
	lifecycle := "-"
	if workspace.Spec.Lifecycle != nil {
		lifecycle = fmt.Sprintf("%d initialize, %d before-stop", len(workspace.Spec.Lifecycle.Initialize), len(workspace.Spec.Lifecycle.BeforeStop))
	}

	return []clioutput.Field{
		{Name: "Name", Value: workspace.Name},
		{Name: "Namespace", Value: workspace.Namespace},
		{Name: "Created", Value: clioutput.Timestamp(workspace.CreationTimestamp)},
		{Name: "Desired state", Value: clioutput.ValueOrDash(string(workspace.Spec.DesiredState))},
		{Name: "Ready", Value: meta.IsStatusConditionTrue(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)},
		{Name: "Generated", Value: workspace.Spec.Generated},
		{Name: "Environment", Value: environment},
		{Name: "Source environment revision", Value: workspace.Status.SourceEnvironmentRevision},
		{Name: "Configured image", Value: clioutput.ValueOrDash(workspace.Spec.Image)},
		{Name: "Runtime image", Value: clioutput.ValueOrDash(workspace.Status.RuntimeImage)},
		{Name: "Storage", Value: storage},
		{Name: "Home volume", Value: clioutput.ValueOrDash(workspace.Status.HomeVolumeClaimName)},
		{Name: "Runtime Pod", Value: clioutput.ValueOrDash(workspace.Status.RuntimePodName)},
		{Name: "Default working directory", Value: clioutput.ValueOrDash(workspace.Spec.DefaultWorkingDirectory)},
		{Name: "Service account", Value: clioutput.ValueOrDash(workspace.Spec.ServiceAccountName)},
		{Name: "Automount service account token", Value: automountToken},
		{Name: "Runtime class", Value: runtimeClass},
		{Name: "Idle timeout", Value: idleTimeout},
		{Name: "Mounts", Value: workspaceMountSummary(workspace.Spec.Mounts)},
		{Name: "Agent credentials", Value: workspaceReferenceNames(workspace.Spec.AgentCredentialRefs)},
		{Name: "Credentials", Value: workspaceReferenceNames(workspace.Spec.CredentialRefs)},
		{Name: "ConfigMaps", Value: workspaceReferenceNames(workspace.Spec.ConfigMapRefs)},
		{Name: "Secrets", Value: workspaceReferenceNames(workspace.Spec.SecretRefs)},
		{Name: "Lifecycle", Value: lifecycle},
		{Name: "Last auto-suspend", Value: clioutput.OptionalTimestamp(workspace.Status.LastAutoSuspendTime)},
		{Name: "Conditions", Value: clioutput.Conditions(workspace.Status.Conditions)},
	}
}

func workspaceStorageSummary(storage workspacesv1alpha1.PersistentStorageSpec) string {
	modes := make([]string, len(storage.AccessModes))
	for index, mode := range storage.AccessModes {
		modes[index] = string(mode)
	}
	return fmt.Sprintf("class=%s, size=%s, access=%s", clioutput.ValueOrDash(storage.StorageClassName), storage.Size.String(), clioutput.ValueOrDash(strings.Join(modes, ",")))
}

func workspaceMountSummary(mounts []workspacesv1alpha1.WorkspaceMount) string {
	summaries := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		source := "-"
		if mount.WorktreeRef != nil {
			source = "worktree/" + mount.WorktreeRef.Name
		}
		if mount.RepositoryRef != nil {
			source = "repository/" + mount.RepositoryRef.Name
		}
		summaries = append(summaries, fmt.Sprintf("%s=%s@%s (readOnly=%t)", mount.Name, source, mount.Path, mount.ReadOnly))
	}

	return clioutput.ValueOrDash(strings.Join(summaries, ", "))
}

func workspaceReferenceNames(references []workspacesv1alpha1.LocalReference) string {
	names := make([]string, len(references))
	for index, reference := range references {
		names[index] = reference.Name
	}

	return clioutput.ValueOrDash(strings.Join(names, ", "))
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
