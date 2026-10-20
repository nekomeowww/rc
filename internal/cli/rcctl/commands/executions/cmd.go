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

package executions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/wait"
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

type runOptions struct {
	placement        command.PlacementOptions
	workspace        string
	createWorkspace  bool
	remove           bool
	retain           bool
	lifecycle        command.LifecycleOptions
	name             string
	interactive      bool
	tty              bool
	detach           bool
	environment      string
	repositories     []string
	worktrees        []string
	agentCredentials []string
	credentials      []string
	environmentVars  []string
	environmentFiles []string
	noPassthrough    bool
	cwd              string
	image            string
	storageClass     string
	size             string
	serviceAccount   string
	noServiceAccount bool
	gpu              command.GPUOptions
	npmRegistry      string
}

type listOptions struct {
	workspace     string
	phase         string
	agent         string
	idPrefix      string
	allNamespaces bool
	all           bool
	output        clioutput.Options
}

// Register attaches execution commands directly to rcctl.
func Register(root *cobra.Command, kubeconfigFlags *kubeconfig.Flags) {
	commands := []*cobra.Command{
		newRunCommand(kubeconfigFlags, true), newRunCommand(kubeconfigFlags, false),
		newAttachCommand(kubeconfigFlags), newListCommand(kubeconfigFlags),
		newInspectCommand(kubeconfigFlags), newLogsCommand(kubeconfigFlags),
		newStopCommand(kubeconfigFlags), newRemoveCommand(kubeconfigFlags),
	}
	for _, cmd := range commands {
		cmd.GroupID = command.ExecutionsGroup
		root.AddCommand(cmd)
	}
}

// newRunCommand routes run to a new Workspace and exec to an existing Workspace.
// Both use runProcess to create the execution and optionally attach its streams.
func newRunCommand(kubeconfigFlags *kubeconfig.Flags, createWorkspace bool) *cobra.Command {
	options := &runOptions{createWorkspace: createWorkspace}
	cmd := &cobra.Command{
		Use:   "run [flags] -- COMMAND [ARG...]",
		Short: "Run a command in a new Workspace (unnamed runs are temporary)",
		Long:  "Run a command in a new Workspace. Unnamed runs delete the Workspace and owned storage after all processes exit. Use --retain to keep it, or workspace create for a persistent development machine. Named runs retain by default. Use --rm to delete a named Workspace after execution.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !createWorkspace {
				options.workspace = args[0]
				args = args[1:]
				// With interspersed flag parsing disabled, a separator after
				// WORKSPACE is part of the positional arguments.
				if len(args) > 0 && args[0] == "--" {
					args = args[1:]
				}
				if len(args) == 0 || args[0] == "" {
					return fmt.Errorf("exec requires WORKSPACE and COMMAND")
				}
			}
			return runProcess(cmd, kubeconfigFlags, args, *options)
		},
	}
	if !createWorkspace {
		cmd.Use = "exec [flags] WORKSPACE -- COMMAND [ARG...]"
		cmd.Short = "Run a command in an existing Workspace"
		cmd.Long = cmd.Short
		cmd.Args = cobra.MinimumNArgs(2)
	}
	addRunFlags(cmd, options)
	cmd.Flags().BoolVarP(&options.detach, "detach", "d", false, "Start without attaching")
	cmd.Flags().BoolVarP(&options.interactive, "interactive", "i", false, "Forward standard input to the process")
	cmd.Flags().BoolVarP(&options.tty, "tty", "t", false, "Allocate a terminal")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func addRunFlags(cmd *cobra.Command, options *runOptions) {
	options.placement.AddFlags(cmd.Flags())
	if options.createWorkspace {
		cmd.Flags().BoolVar(&options.remove, "rm", false, "Delete after all processes terminate (default for unnamed runs); --rm=false preserves legacy retention")
		cmd.Flags().BoolVar(&options.retain, "retain", false, "Keep the Workspace after processes exit; idle lifecycle policy still applies")
		cmd.MarkFlagsMutuallyExclusive("rm", "retain")
		options.lifecycle.AddFlags(cmd.Flags())
		cmd.Flags().StringVar(&options.name, "name", "", "Name for the new Workspace; named runs retain by default (use --rm for cleanup)")
	}
	cmd.Flags().StringVar(&options.environment, "environment", "", "WorkspaceEnvironment for a new Workspace or existing-target requirement")
	cmd.Flags().StringArrayVar(&options.repositories, "repo", nil, "Repository requirement or new writable Worktree source; repeat")
	cmd.Flags().StringArrayVar(&options.worktrees, "worktree", nil, "Worktree requirement or new Workspace mount; repeat")
	cmd.Flags().StringArrayVar(&options.agentCredentials, "agent-credential", nil, "Ordered AgentCredential names; repeat")
	cmd.Flags().StringArrayVar(&options.credentials, "credential", nil, "Credential names to project into the process; repeat")
	cmd.Flags().StringArrayVar(&options.environmentVars, "env", nil, "Explicit NAME or NAME=value; repeat")
	cmd.Flags().StringArrayVar(&options.environmentFiles, "env-file", nil, "Read environment values from a file; repeat")
	cmd.Flags().BoolVar(&options.noPassthrough, "no-env-passthrough", false, "Disable caller environment pass-through")
	cmd.Flags().StringVar(&options.cwd, "cwd", "", "Working directory")
	if options.createWorkspace {
		cmd.Flags().StringVar(&options.image, "image", "", "Runner image for a new blank Workspace")
		cmd.Flags().StringVar(&options.storageClass, "storage-class", "", "StorageClass for a new blank Workspace")
		cmd.Flags().StringVar(&options.size, "size", "20Gi", "Home volume size for a new blank Workspace")
		cmd.Flags().StringVar(&options.serviceAccount, "service-account", "", "Same-namespace ServiceAccount for a new Workspace")
		cmd.Flags().BoolVar(&options.noServiceAccount, "no-service-account", false, "Disable ServiceAccount token mounting for a new Workspace")
	}
	cmd.Flags().StringVar(&options.npmRegistry, "npm-registry", "", "npm registry URL for this command")
	options.gpu.AddFlags(cmd.Flags())
}

//nolint:gocyclo // This command coordinates target, credential, environment, and terminal setup.
func runProcess(cmd *cobra.Command, kubeconfigFlags *kubeconfig.Flags, argv []string, options runOptions) (returnedErr error) {
	runRequest, err := options.targetRequest(cmd.Flags())
	if err != nil {
		return err
	}
	registryEnvironment, err := workspaceservice.NPMRegistryEnvironment(options.npmRegistry)
	if err != nil {
		return err
	}
	var files []map[string]string
	var values map[string]string
	environmentPrepared := false
	if len(registryEnvironment) > 0 {
		files, err = workspaceservice.ReadEnvironmentFiles(options.environmentFiles)
		if err != nil {
			return err
		}
		if err := workspaceservice.ValidateNPMRegistryEnvironmentConflicts(files, options.environmentVars, registryEnvironment); err != nil {
			return err
		}
		values, err = workspaceservice.BuildProcessEnvironment(workspaceservice.EnvironmentOptions{
			Caller: os.Environ(), NoPassthrough: options.noPassthrough, Files: files, Explicit: options.environmentVars, Lookup: os.LookupEnv,
		})
		if err != nil {
			return err
		}
		workspaceservice.AddNPMRegistryEnvironment(values, registryEnvironment)
		environmentPrepared = true
	}
	osName, tolerations, placementErr := options.placement.Resolve()
	if placementErr != nil {
		return placementErr
	}
	resources, err := options.gpu.ResourceRequirements(cmd.Flags().Changed("gpu"), cmd.Flags().Changed("gpu-vram"))
	if err != nil {
		return err
	}
	config, namespace, contextName, err := kubeconfigFlags.ResolveWithIdentity()
	if err != nil {
		return err
	}
	clusterClient, err := cluster.New(config)
	if err != nil {
		return err
	}
	defaults, err := loadDefaults(contextName, namespace)
	if err != nil {
		return err
	}
	agentType := workspaceservice.AgentTypeForCommand(argv[0])
	repositories, err := resolveRepositories(cmd.Context(), clusterClient.Kube, namespace, options.repositories)
	if err != nil {
		return err
	}
	worktrees, err := resolveWorktrees(cmd.Context(), clusterClient.Kube, namespace, options.worktrees)
	if err != nil {
		return err
	}
	runRequest.OS = osName
	runRequest.NodeSelector = options.placement.NodeSelector
	runRequest.Tolerations = tolerations
	runRequest.Namespace = namespace
	runRequest.DefaultWorkspace = defaults.Workspace
	runRequest.DefaultEnvironment = defaults.Environment
	runRequest.Repositories = repositories
	runRequest.Worktrees = worktrees
	credentialRefs := options.credentials
	credentialNames, agentCredential, selectedAgentType, err := selectAgentCredentials(
		cmd.Context(), clusterClient.Kube, namespace, agentType, options.agentCredentials, options.createWorkspace,
	)
	if err != nil {
		return err
	}
	if selectedAgentType != "" {
		agentType = selectedAgentType
	}
	var storage *workspacesv1alpha1.PersistentStorageSpec
	if options.createWorkspace {
		storage, err = temporaryStorage(options)
		if err != nil {
			return err
		}
	}
	automount := (*bool)(nil)
	if options.noServiceAccount {
		if options.serviceAccount != "" {
			return fmt.Errorf("--no-service-account and --service-account are mutually exclusive")
		}
		automount = boolPointer(false)
	}
	runRequest.AgentCredentialRefs = credentialNames
	runRequest.CredentialRefs = credentialRefs
	runRequest.Storage = storage
	runRequest.Image = options.image
	runRequest.Resources = resources
	runRequest.ServiceAccountName = options.serviceAccount
	runRequest.AutomountServiceAccountToken = automount
	runRequest.NamePrefix = agentType
	routeNPMRegistryEnvironment(options.createWorkspace, &runRequest, values, registryEnvironment)
	target, err := (&workspaceservice.Runner{Client: clusterClient.Kube}).Prepare(cmd.Context(), runRequest)
	if err != nil {
		return err
	}
	cleanupTemporaryOnReturn := target.Created && target.Workspace.Spec.IsTemporary()
	defer func() {
		if !cleanupTemporaryOnReturn {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), 30*time.Second)
		defer cancel()
		if err := client.IgnoreNotFound(clusterClient.Kube.Delete(cleanupContext, target.Workspace)); err != nil {
			returnedErr = errors.Join(returnedErr, fmt.Errorf("delete new Workspace %q: %w", target.Workspace.Name, err))
		}
	}()
	if target.Created {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), runLifecycleNotice(target.Workspace)); err != nil {
			return err
		}
		indicator := progress.Start(cmd.ErrOrStderr(), "preparing Workspace...")
		if err := waitWorkspaceReady(cmd.Context(), clusterClient.Kube, target.Workspace); err != nil {
			indicator.Stop()
			return err
		}
		indicator.Stop()
	}
	if !environmentPrepared {
		files, err = workspaceservice.ReadEnvironmentFiles(options.environmentFiles)
		if err != nil {
			return err
		}
		values, err = workspaceservice.BuildProcessEnvironment(workspaceservice.EnvironmentOptions{
			Caller: os.Environ(), NoPassthrough: options.noPassthrough, Files: files, Explicit: options.environmentVars, Lookup: os.LookupEnv,
		})
		if err != nil {
			return err
		}
	}
	processClient := &workspaceservice.ProcessClient{Kube: clusterClient.Kube, Runtime: clusterClient.Processes, Config: config}
	process, err := processClient.Start(cmd.Context(), workspaceservice.ProcessStartRequest{
		Namespace: namespace, Target: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: target.Workspace.Name},
		Command: argv, WorkingDirectory: options.cwd, TTY: options.tty, AgentType: agentType,
		AgentCredential: agentCredential, Credentials: credentialRefs, Environment: values,
	})
	if err != nil {
		return err
	}
	cleanupTemporaryOnReturn = false
	if _, err := fmt.Fprintln(cmd.ErrOrStderr(), process.Name); err != nil {
		return err
	}
	indicator := progress.Start(cmd.ErrOrStderr(), "starting WorkspaceExec...")
	ready, err := processClient.WaitUntilAttachable(cmd.Context(), process)
	indicator.Stop()
	if err != nil {
		return err
	}
	if options.detach && !ready.Status.Phase.Terminal() {
		return nil
	}
	if ready.Status.Phase.Terminal() {
		if err := processClient.Logs(cmd.Context(), ready, cmd.OutOrStdout()); err != nil {
			return err
		}
	} else {
		var input io.Reader
		if options.interactive {
			input = cmd.InOrStdin()
		}
		if err := processClient.Attach(cmd.Context(), ready, input, cmd.OutOrStdout()); err != nil && cmd.Context().Err() == nil {
			return err
		}
	}
	finished := ready
	if !ready.Status.Phase.Terminal() {
		finished, err = processClient.WaitUntilTerminal(cmd.Context(), process)
		if err != nil {
			return err
		}
	}
	return workspaceservice.ResultError(finished)
}

// targetRequest translates CLI creation intent once, before any cluster access.
// It preserves the legacy explicit --rm=false form alongside --retain.
func (options runOptions) targetRequest(flags *pflag.FlagSet) (workspaceservice.RunRequest, error) {
	if err := options.lifecycle.Validate(); err != nil {
		return workspaceservice.RunRequest{}, err
	}
	var retention workspacesv1alpha1.WorkspaceRetentionPolicy
	if flags.Changed("rm") {
		retention = workspacesv1alpha1.WorkspaceRetentionPolicyRetain
		if options.remove {
			retention = workspacesv1alpha1.WorkspaceRetentionPolicyDeleteAfterProcessesExit
		}
	} else if options.retain {
		retention = workspacesv1alpha1.WorkspaceRetentionPolicyRetain
	}
	return workspaceservice.RunRequest{
		Create: options.createWorkspace, Workspace: options.workspace, Name: options.name,
		RetentionPolicy:      retention,
		Environment:          options.environment,
		IdleTimeout:          command.Duration(options.lifecycle.IdleTimeout),
		DeleteAfterSuspended: command.Duration(options.lifecycle.DeleteAfterSuspended),
	}, nil
}

// runLifecycleNotice keeps command stdout clean and makes cleanup visible to
// both interactive users and automation before the execution starts.
func runLifecycleNotice(workspace *workspacesv1alpha1.Workspace) string {
	message := fmt.Sprintf("workspace/%s created; retention=%s", workspace.Name, workspace.Spec.EffectiveRetentionPolicy())
	if workspace.Spec.IsTemporary() {
		return message + "; Workspace and owned storage will be deleted 5m after all processes exit (--retain to keep)"
	}

	return message + durationNotice("idleTimeout", workspace.Spec.IdleTimeout) +
		durationNotice("deleteAfterSuspended", workspace.Spec.DeleteAfterSuspended)
}

// durationNotice renders an optional lifecycle clock as a notice suffix.
func durationNotice(name string, value *metav1.Duration) string {
	if value == nil {
		return ""
	}
	return "; " + name + "=" + value.Duration.String()
}

func routeNPMRegistryEnvironment(temporary bool, request *workspaceservice.RunRequest, processEnvironment map[string]string, registry []corev1.EnvVar) {
	if len(registry) == 0 {
		return
	}
	if temporary {
		request.Env = append([]corev1.EnvVar(nil), registry...)
		workspaceservice.RemoveNPMRegistryEnvironment(processEnvironment)
		return
	}
	for _, variable := range registry {
		processEnvironment[variable.Name] = variable.Value
	}
}

func loadDefaults(contextName string, namespace string) (workspaceservice.Defaults, error) {
	path, err := workspaceservice.DefaultConfigPath()
	if err != nil {
		return workspaceservice.Defaults{}, err
	}

	return (workspaceservice.DefaultStore{Path: path}).Get(contextName, namespace)
}

func selectAgentCredentials(ctx context.Context, kubeClient client.Client, namespace string, agentType string, explicit []string, temporary bool) ([]string, string, string, error) {
	if len(explicit) > 0 {
		selectedName := ""
		selectedType := ""
		for _, name := range explicit {
			credential := new(configsv1alpha1.AgentCredential)
			if err := kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, credential); err != nil {
				return nil, "", "", fmt.Errorf("get AgentCredential %q: %w", name, err)
			}
			credentialType := string(credential.Spec.Agent)
			if selectedName == "" && (agentType == "" || credentialType == agentType) {
				selectedName = name
				selectedType = credentialType
			}
		}
		if selectedName == "" {
			return nil, "", "", fmt.Errorf("none of the selected AgentCredentials are for %s", agentType)
		}
		return append([]string(nil), explicit...), selectedName, selectedType, nil
	}
	if agentType == "" || !temporary {
		return nil, "", agentType, nil
	}
	list := new(configsv1alpha1.AgentCredentialList)
	if err := kubeClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, "", "", fmt.Errorf("list compatible AgentCredentials: %w", err)
	}
	matches := make([]string, 0, len(list.Items))
	for _, credential := range list.Items {
		if string(credential.Spec.Agent) == agentType {
			matches = append(matches, credential.Name)
		}
	}
	if len(matches) > 1 {
		return nil, "", "", fmt.Errorf("multiple %s AgentCredentials exist; select an ordered credential with --agent-credential", agentType)
	}
	selected := ""
	if len(matches) == 1 {
		selected = matches[0]
	}
	return matches, selected, agentType, nil
}

func resolveRepositories(ctx context.Context, kubeClient client.Client, namespace string, selectors []string) ([]workspaceservice.MountRequest, error) {
	result := make([]workspaceservice.MountRequest, 0, len(selectors))
	for _, selector := range selectors {
		repository, err := repositoryservice.ResolveRepository(ctx, kubeClient, namespace, selector)
		if err != nil {
			return nil, err
		}
		result = append(result, workspaceservice.MountRequest{Name: repository.Name})
	}

	return result, nil
}

func resolveWorktrees(ctx context.Context, kubeClient client.Client, namespace string, selectors []string) ([]workspaceservice.MountRequest, error) {
	result := make([]workspaceservice.MountRequest, 0, len(selectors))
	for _, selector := range selectors {
		name := selector
		repositorySelector := ""
		if slash := strings.LastIndex(name, "/"); slash >= 0 {
			repositorySelector = name[:slash]
			name = name[slash+1:]
		}
		worktree := new(repositoriesv1alpha1.Worktree)
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, worktree); err != nil {
			return nil, fmt.Errorf("get Worktree %q: %w", name, err)
		}
		if repositorySelector != "" {
			repository, err := repositoryservice.ResolveRepository(ctx, kubeClient, namespace, repositorySelector)
			if err != nil {
				return nil, err
			}
			if worktree.Spec.RepositoryRef.Name != repository.Name {
				return nil, fmt.Errorf("worktree %q belongs to Repository %q, not %q", name, worktree.Spec.RepositoryRef.Name, repository.Name)
			}
		}
		result = append(result, workspaceservice.MountRequest{Name: name})
	}

	return result, nil
}

func temporaryStorage(options runOptions) (*workspacesv1alpha1.PersistentStorageSpec, error) {
	size, err := resource.ParseQuantity(options.size)
	if err != nil || size.Sign() <= 0 {
		return nil, fmt.Errorf("parse --size: value must be a positive Kubernetes quantity")
	}

	return &workspacesv1alpha1.PersistentStorageSpec{StorageClassName: options.storageClass, Size: size}, nil
}

func waitWorkspaceReady(ctx context.Context, kubeClient client.Client, workspace *workspacesv1alpha1.Workspace) error {
	return wait.PollUntilContextCancel(ctx, 300*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current := new(workspacesv1alpha1.Workspace)
		if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
			return false, err
		}
		if meta.IsStatusConditionTrue(current.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady) {
			return true, nil
		}
		if condition := meta.FindStatusCondition(current.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady); condition != nil && condition.Status == "False" && condition.Reason == "InvalidSpec" {
			return false, fmt.Errorf("workspace %q is invalid: %s", current.Name, condition.Message)
		}
		return false, nil
	})
}

func newAttachCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "attach ID", Short: "Attach another terminal to the original live process", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			processClient, namespace, err := processClient(kubeconfigFlags)
			if err != nil {
				return err
			}
			process, err := processClient.Resolve(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}
			if process.Status.Phase != workspacesv1alpha1.WorkspaceExecPhaseRunning {
				return fmt.Errorf("cannot connect to WorkspaceExec %s in phase %s", process.Name, process.Status.Phase)
			}
			if err := processClient.Attach(cmd.Context(), process, cmd.InOrStdin(), cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("cannot connect to original WorkspaceExec %s: %w", process.Name, err)
			}
			return nil
		},
	}
}

func newLogsCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "logs ID", Short: "Read the persistent process transcript", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			processClient, namespace, err := processClient(kubeconfigFlags)
			if err != nil {
				return err
			}
			process, err := processClient.Resolve(cmd.Context(), namespace, args[0])
			if errors.Is(err, workspaceservice.ErrProcessRecordNotFound) {
				inventory := processInventory(processClient)
				found, findErr := inventory.Find(cmd.Context(), namespace, args[0])
				if findErr != nil {
					return findErr
				}
				target, targetErr := inventory.RuntimeTarget(cmd.Context(), found.Target)
				if targetErr != nil {
					return targetErr
				}
				return processClient.Runtime.Logs(cmd.Context(), target, found.ID, cmd.OutOrStdout())
			}
			if err != nil {
				return err
			}
			return processClient.Logs(cmd.Context(), process, cmd.OutOrStdout())
		},
	}
}

func newStopCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "stop ID", Short: "Request one-way process termination", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			processClient, namespace, err := processClient(kubeconfigFlags)
			if err != nil {
				return err
			}
			process, err := processClient.Resolve(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}
			if err := processClient.Stop(cmd.Context(), process); err != nil {
				return err
			}
			indicator := progress.Start(cmd.ErrOrStderr(), "stopping WorkspaceExec...")
			defer indicator.Stop()
			_, err = processClient.WaitUntilTerminal(cmd.Context(), process)
			return err
		},
	}
}

func newRemoveCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "rm ID", Short: "Request deletion of a completed execution and its transcript", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			processClient, namespace, err := processClient(kubeconfigFlags)
			if err != nil {
				return err
			}
			process, err := processClient.Resolve(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}
			if !process.Status.Phase.Terminal() {
				return fmt.Errorf("process %s is still %s; stop it first", process.Name, process.Status.Phase)
			}
			return processClient.Kube.Delete(cmd.Context(), process)
		},
	}
}

func newListCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := new(listOptions)
	cmd := &cobra.Command{
		Use: "ps", Short: "List running processes", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := options.output.Validate(true); err != nil {
				return err
			}
			clusterClient, namespace, err := cluster.Connect(kubeconfigFlags)
			if err != nil {
				return err
			}
			if options.allNamespaces {
				namespace = ""
			}
			inventory := &workspaceservice.ProcessInventory{Kube: clusterClient.Kube, History: clusterClient.Processes}
			processes, warnings, err := inventory.List(cmd.Context(), workspaceservice.InventoryRequest{Namespace: namespace, Workspace: options.workspace})
			if err != nil {
				return err
			}
			for _, warning := range warnings {
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", warning); err != nil {
					return err
				}
			}
			processes = processListItems(processes, *options)
			return options.output.PrintValue(
				cmd.OutOrStdout(), processList{Items: processes},
				processListTable(processes, options.allNamespaces, time.Now()),
			)
		},
	}
	cmd.Flags().BoolVarP(&options.all, "all", "a", false, "Include finished processes")
	cmd.Flags().StringVar(&options.workspace, "workspace", "", "Filter by target Workspace")
	cmd.Flags().StringVar(&options.phase, "phase", "", "Filter by process phase")
	cmd.Flags().StringVar(&options.agent, "agent", "", "Filter by recognized agent type")
	cmd.Flags().StringVar(&options.idPrefix, "id-prefix", "", "Filter by managed ID prefix")
	cmd.Flags().BoolVarP(&options.allNamespaces, "all-namespaces", "A", false, "List across all permitted namespaces")
	options.output.AddFlags(cmd, true)

	return cmd
}

func newInspectCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := new(clioutput.Options)
	cmd := &cobra.Command{
		Use: "inspect ID", Short: "Show a process record", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := options.Validate(false); err != nil {
				return err
			}
			processClient, namespace, err := processClient(kubeconfigFlags)
			if err != nil {
				return err
			}
			process, err := processClient.Resolve(cmd.Context(), namespace, args[0])
			if errors.Is(err, workspaceservice.ErrProcessRecordNotFound) {
				found, findErr := processInventory(processClient).Find(cmd.Context(), namespace, args[0])
				if findErr != nil {
					return findErr
				}
				return options.PrintValueDetails(cmd.OutOrStdout(), found, historyDetailFields(found))
			}
			if err != nil {
				return err
			}
			return options.PrintDetails(cmd.OutOrStdout(), process, processClient.Kube.Scheme(), processDetailFields(process))
		},
	}
	options.AddFlags(cmd, false)

	return cmd
}

const fieldTTY = "TTY"

// processList is the structured ps output: what each supervisor reports.
type processList struct {
	Items []workspaceservice.InventoryProcess `json:"items"`
}

func processListItems(processes []workspaceservice.InventoryProcess, options listOptions) []workspaceservice.InventoryProcess {
	items := make([]workspaceservice.InventoryProcess, 0, len(processes))
	for _, process := range processes {
		phase := process.DisplayPhase()
		if !options.all && options.phase == "" && phase != string(workspacesv1alpha1.WorkspaceExecPhaseRunning) {
			continue
		}
		if options.phase != "" && !strings.EqualFold(phase, options.phase) {
			continue
		}
		if options.agent != "" && process.Agent != options.agent {
			continue
		}
		if options.idPrefix != "" && !strings.HasPrefix(process.ID, options.idPrefix) {
			continue
		}
		items = append(items, process)
	}

	return items
}

func processListTable(processes []workspaceservice.InventoryProcess, includeNamespace bool, now time.Time) clioutput.Table {
	columns := make([]clioutput.Column, 0, 10)
	if includeNamespace {
		columns = append(columns, clioutput.Column{Name: "NAMESPACE", MaxWidth: 24})
	}
	columns = append(columns,
		clioutput.Column{Name: "ID", MaxWidth: 32},
		clioutput.Column{Name: "WORKSPACE", MaxWidth: 24},
		clioutput.Column{Name: "COMMAND", MinWidth: 12, MaxWidth: 48, Flexible: true},
		clioutput.Column{Name: fieldTTY, Wide: true},
		clioutput.Column{Name: "AGENT", MaxWidth: 16, Wide: true},
		clioutput.Column{Name: "STATUS"},
		clioutput.Column{Name: "CLIENTS", Wide: true},
		clioutput.Column{Name: "AGE"},
		clioutput.Column{Name: "EXIT", Wide: true},
	)
	rows := make([][]any, 0, len(processes))
	for _, process := range processes {
		exit := "-"
		if process.ExitCode != nil {
			exit = fmt.Sprint(*process.ExitCode)
		}
		age := "<unknown>"
		if process.StartedAt != nil {
			age = duration.HumanDuration(now.Sub(*process.StartedAt))
		}
		row := make([]any, 0, len(columns))
		if includeNamespace {
			row = append(row, process.Target.Namespace)
		}
		target := process.Target.Name
		if process.Target.Kind == workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment {
			target = "env/" + target
		}
		row = append(row,
			process.ID,
			target,
			strings.Join(process.Command, " "),
			process.TTY,
			clioutput.ValueOrDash(process.Agent),
			clioutput.ValueOrDash(process.DisplayPhase()),
			process.AttachedClients,
			age,
			exit,
		)
		rows = append(rows, row)
	}

	return clioutput.Table{Columns: columns, Rows: rows}
}

func processInventory(processes *workspaceservice.ProcessClient) *workspaceservice.ProcessInventory {
	return &workspaceservice.ProcessInventory{Kube: processes.Kube, History: processes.Runtime}
}

// historyDetailFields describes a process known only from its supervisor.
func historyDetailFields(process workspaceservice.InventoryProcess) []clioutput.Field {
	completed := "-"
	if process.CompletedAt != nil {
		completed = process.CompletedAt.UTC().Format(time.RFC3339)
	}
	started := "-"
	if process.StartedAt != nil {
		started = process.StartedAt.UTC().Format(time.RFC3339)
	}

	return []clioutput.Field{
		{Name: "Name", Value: process.ID},
		{Name: "Namespace", Value: process.Target.Namespace},
		{Name: "Target", Value: fmt.Sprintf("%s/%s", process.Target.Kind, process.Target.Name)},
		{Name: "Command", Value: quotedArguments(process.Command)},
		{Name: "Working directory", Value: clioutput.ValueOrDash(process.WorkingDirectory)},
		{Name: fieldTTY, Value: process.TTY},
		{Name: "Agent", Value: clioutput.ValueOrDash(process.Agent)},
		{Name: "Phase", Value: clioutput.ValueOrDash(process.DisplayPhase())},
		{Name: "Started", Value: started},
		{Name: "Completed", Value: completed},
		{Name: "Exit code", Value: exitCodeOrDash(process.ExitCode)},
		{Name: "Termination reason", Value: clioutput.ValueOrDash(process.Reason)},
		{Name: "Record", Value: "supervisor history (no WorkspaceExec)"},
	}
}

func processDetailFields(process *workspacesv1alpha1.WorkspaceExec) []clioutput.Field {
	return []clioutput.Field{
		{Name: "Name", Value: process.Name},
		{Name: "Namespace", Value: process.Namespace},
		{Name: "Created", Value: clioutput.Timestamp(process.CreationTimestamp)},
		{Name: "Target", Value: fmt.Sprintf("%s/%s", process.Spec.TargetRef.Kind, process.Spec.TargetRef.Name)},
		{Name: "Command", Value: quotedArguments(process.Spec.Command)},
		{Name: "Working directory", Value: clioutput.ValueOrDash(process.Spec.WorkingDirectory)},
		{Name: fieldTTY, Value: process.Spec.TTY},
		{Name: "Desired state", Value: clioutput.ValueOrDash(string(process.Spec.DesiredState))},
		{Name: "Agent", Value: clioutput.ValueOrDash(process.Spec.AgentType)},
		{Name: "Agent credential", Value: localReferenceName(process.Spec.AgentCredentialRef)},
		{Name: "Credentials", Value: localReferenceNames(process.Spec.CredentialRefs)},
		{Name: "Environment", Value: processEnvironmentNames(process.Spec.Env)},
		{Name: "Phase", Value: clioutput.ValueOrDash(string(process.Status.Phase))},
		{Name: "Attached clients", Value: process.Status.AttachedClients},
		{Name: "Runtime Pod", Value: clioutput.ValueOrDash(process.Status.RuntimePodName)},
		{Name: "Started", Value: clioutput.OptionalTimestamp(process.Status.StartedAt)},
		{Name: "Completed", Value: clioutput.OptionalTimestamp(process.Status.CompletedAt)},
		{Name: "Exit code", Value: exitCodeOrDash(process.Status.ExitCode)},
		{Name: "Termination reason", Value: clioutput.ValueOrDash(process.Status.TerminationReason)},
		{Name: "Transcript", Value: clioutput.ValueOrDash(process.Status.TranscriptPath)},
		{Name: "Conditions", Value: clioutput.Conditions(process.Status.Conditions)},
	}
}

func quotedArguments(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = strconv.Quote(argument)
	}

	return strings.Join(quoted, " ")
}

func localReferenceName(reference *workspacesv1alpha1.LocalReference) string {
	if reference == nil {
		return "-"
	}

	return clioutput.ValueOrDash(reference.Name)
}

func localReferenceNames(references []workspacesv1alpha1.LocalReference) string {
	names := make([]string, 0, len(references))
	for _, reference := range references {
		names = append(names, reference.Name)
	}

	return clioutput.ValueOrDash(strings.Join(names, ", "))
}

func processEnvironmentNames(environment []workspacesv1alpha1.ProcessEnvironmentVariable) string {
	names := make([]string, 0, len(environment))
	for _, variable := range environment {
		name := variable.Name
		if variable.Key != "" && variable.Key != variable.Name {
			name += "=" + variable.Key
		}
		names = append(names, name)
	}

	return clioutput.ValueOrDash(strings.Join(names, ", "))
}

func exitCodeOrDash(exitCode *int32) string {
	if exitCode == nil {
		return "-"
	}

	return fmt.Sprint(*exitCode)
}

func processClient(kubeconfigFlags *kubeconfig.Flags) (*workspaceservice.ProcessClient, string, error) {
	clusterClient, namespace, err := cluster.Connect(kubeconfigFlags)
	if err != nil {
		return nil, "", err
	}

	return &workspaceservice.ProcessClient{Kube: clusterClient.Kube, Runtime: clusterClient.Processes, Config: clusterClient.Config}, namespace, nil
}

func boolPointer(value bool) *bool {
	return &value
}

// processListSelectors pushes supported filters to the API server so default
// ps transfers running processes instead of the entire retained history.
