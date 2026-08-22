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

package agents

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
)

type runOptions struct {
	workspace        string
	temporary        bool
	environment      string
	repositories     []string
	worktrees        []string
	credentials      []string
	genericCreds     []string
	environmentVars  []string
	environmentFiles []string
	noPassthrough    bool
	cwd              string
	image            string
	storageClass     string
	size             string
	serviceAccount   string
	noServiceAccount bool
}

type listOptions struct {
	workspace     string
	phase         string
	agent         string
	idPrefix      string
	allNamespaces bool
}

// Register attaches Agent Process commands to rcctl.
func Register(root *cobra.Command, kubeconfigFlags *kubeconfig.Flags) {
	root.AddCommand(NewCommand(kubeconfigFlags))
}

// NewCommand returns the Agent Process command group.
func NewCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	root := &cobra.Command{Use: "agent", Aliases: []string{"agents"}, Short: "Run and reconnect to persistent Agent Processes", GroupID: command.AgentsGroup}
	root.AddCommand(newRunCommand(kubeconfigFlags, true), newRunCommand(kubeconfigFlags, false), newResumeCommand(kubeconfigFlags), newListCommand(kubeconfigFlags), newLogsCommand(kubeconfigFlags), newStopCommand(kubeconfigFlags), newDeleteCommand(kubeconfigFlags))

	return root
}

func newRunCommand(kubeconfigFlags *kubeconfig.Flags, tty bool) *cobra.Command {
	options := new(runOptions)
	verb := "exec"
	short := "Run a command and wait for its exit status"
	if tty {
		verb = "run"
		short = "Run and attach to an interactive Agent Process"
	}
	cmd := &cobra.Command{
		Use: verb + " [flags] [--] COMMAND [ARG...]", Short: short, Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProcess(cmd, kubeconfigFlags, args, tty, *options)
		},
	}
	addRunFlags(cmd, options)
	cmd.Flags().SetInterspersed(false)

	return cmd
}

func addRunFlags(cmd *cobra.Command, options *runOptions) {
	cmd.Flags().StringVar(&options.workspace, "workspace", "", "Existing Workspace name")
	cmd.Flags().BoolVar(&options.temporary, "temporary", false, "Create a generated Workspace even when a default exists")
	cmd.Flags().StringVar(&options.environment, "environment", "", "WorkspaceEnvironment for a generated Workspace or existing-target requirement")
	cmd.Flags().StringArrayVar(&options.repositories, "repo", nil, "Repository requirement or generated writable Worktree source; repeat")
	cmd.Flags().StringArrayVar(&options.worktrees, "worktree", nil, "Worktree requirement or generated mount; repeat")
	cmd.Flags().StringArrayVar(&options.credentials, "credential", nil, "Ordered AgentCredential names; repeat")
	cmd.Flags().StringArrayVar(&options.genericCreds, "dangerously-include-credentials", nil, "Generic Credential names to expose; repeat")
	cmd.Flags().StringArrayVar(&options.environmentVars, "env", nil, "Explicit NAME or NAME=value; repeat")
	cmd.Flags().StringArrayVar(&options.environmentFiles, "env-file", nil, "Read environment values from a file; repeat")
	cmd.Flags().BoolVar(&options.noPassthrough, "no-env-passthrough", false, "Disable caller environment pass-through")
	cmd.Flags().StringVar(&options.cwd, "cwd", "", "Working directory")
	cmd.Flags().StringVar(&options.image, "image", "", "Runner image for a generated blank Workspace")
	cmd.Flags().StringVar(&options.storageClass, "storage-class", "", "StorageClass for a generated blank Workspace")
	cmd.Flags().StringVar(&options.size, "size", "20Gi", "Home volume size for a generated blank Workspace")
	cmd.Flags().StringVar(&options.serviceAccount, "service-account", "", "Same-namespace ServiceAccount for a generated Workspace")
	cmd.Flags().BoolVar(&options.noServiceAccount, "no-service-account", false, "Disable ServiceAccount token mounting for a generated Workspace")
}

//nolint:gocyclo // This command coordinates target, credential, environment, and terminal setup.
func runProcess(cmd *cobra.Command, kubeconfigFlags *kubeconfig.Flags, argv []string, tty bool, options runOptions) error {
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
	if agentType == "" {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: command %q has no recognized agent adapter; credentials will not be selected automatically\n", argv[0]); err != nil {
			return err
		}
		if len(options.credentials) > 0 {
			return fmt.Errorf("--credential requires a recognized agent command; use --dangerously-include-credentials for generic credentials")
		}
	}
	credentialNames, err := selectAgentCredentials(cmd.Context(), clusterClient.Kube, namespace, agentType, options.credentials, options.workspace == "" && (options.temporary || defaults.Workspace == ""))
	if err != nil {
		return err
	}
	repositories, err := resolveRepositories(cmd.Context(), clusterClient.Kube, namespace, options.repositories)
	if err != nil {
		return err
	}
	worktrees, err := resolveWorktrees(cmd.Context(), clusterClient.Kube, namespace, options.worktrees)
	if err != nil {
		return err
	}
	storage, err := generatedStorage(options)
	if err != nil {
		return err
	}
	automount := (*bool)(nil)
	if options.noServiceAccount {
		if options.serviceAccount != "" {
			return fmt.Errorf("--no-service-account and --service-account are mutually exclusive")
		}
		automount = boolPointer(false)
	}
	target, err := (&workspaceservice.Runner{Client: clusterClient.Kube}).Prepare(cmd.Context(), workspaceservice.RunRequest{
		Namespace: namespace, Workspace: options.workspace, DefaultWorkspace: defaults.Workspace, Temporary: options.temporary,
		Environment: options.environment, DefaultEnvironment: defaults.Environment,
		Repositories: repositories, Worktrees: worktrees,
		AgentCredentialRefs: credentialNames, CredentialRefs: options.genericCreds,
		Storage: storage, Image: options.image, ServiceAccountName: options.serviceAccount, AutomountServiceAccountToken: automount,
		NamePrefix: agentType,
	})
	if err != nil {
		return err
	}
	if target.Created {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "workspace/%s created\n", target.Workspace.Name); err != nil {
			return err
		}
		indicator := progress.Start(cmd.ErrOrStderr(), "preparing Workspace...")
		if err := waitWorkspaceReady(cmd.Context(), clusterClient.Kube, target.Workspace); err != nil {
			indicator.Stop()
			return err
		}
		indicator.Stop()
	}
	files, err := workspaceservice.ReadEnvironmentFiles(options.environmentFiles)
	if err != nil {
		return err
	}
	values, err := workspaceservice.BuildProcessEnvironment(workspaceservice.EnvironmentOptions{
		Caller: os.Environ(), NoPassthrough: options.noPassthrough, Files: files, Explicit: options.environmentVars, Lookup: os.LookupEnv,
	})
	if err != nil {
		return err
	}
	agentCredential := ""
	if len(credentialNames) > 0 {
		agentCredential = credentialNames[0]
	}
	processClient := &workspaceservice.ProcessClient{Kube: clusterClient.Kube, Runtime: clusterClient.Processes, Config: config}
	process, err := processClient.Start(cmd.Context(), workspaceservice.ProcessStartRequest{
		Namespace: namespace, Target: workspacesv1alpha1.AgentProcessTargetReference{Kind: workspacesv1alpha1.AgentProcessTargetWorkspace, Name: target.Workspace.Name},
		Command: argv, WorkingDirectory: options.cwd, TTY: tty, AgentType: agentType,
		AgentCredential: agentCredential, Credentials: options.genericCreds, Environment: values,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.ErrOrStderr(), process.Name); err != nil {
		return err
	}
	indicator := progress.Start(cmd.ErrOrStderr(), "starting AgentProcess...")
	ready, err := processClient.WaitUntilAttachable(cmd.Context(), process)
	indicator.Stop()
	if err != nil {
		return err
	}
	if processTerminal(ready.Status.Phase) {
		if err := processClient.Logs(cmd.Context(), ready, cmd.OutOrStdout()); err != nil {
			return err
		}
	} else {
		if err := processClient.Attach(cmd.Context(), ready, cmd.InOrStdin(), cmd.OutOrStdout()); err != nil && cmd.Context().Err() == nil {
			return err
		}
	}
	finished := ready
	if !processTerminal(ready.Status.Phase) {
		finished, err = processClient.WaitUntilTerminal(cmd.Context(), process)
		if err != nil {
			return err
		}
	}
	return workspaceservice.ResultError(finished)
}

func loadDefaults(contextName string, namespace string) (workspaceservice.Defaults, error) {
	path, err := workspaceservice.DefaultConfigPath()
	if err != nil {
		return workspaceservice.Defaults{}, err
	}

	return (workspaceservice.DefaultStore{Path: path}).Get(contextName, namespace)
}

func selectAgentCredentials(ctx context.Context, kubeClient client.Client, namespace string, agentType string, explicit []string, generated bool) ([]string, error) {
	if len(explicit) > 0 {
		for _, name := range explicit {
			credential := new(configsv1alpha1.AgentCredential)
			if err := kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, credential); err != nil {
				return nil, fmt.Errorf("get AgentCredential %q: %w", name, err)
			}
			if agentType != "" && string(credential.Spec.Agent) != agentType {
				return nil, fmt.Errorf("agent credential %q is for %s, not %s", name, credential.Spec.Agent, agentType)
			}
		}
		return append([]string(nil), explicit...), nil
	}
	if agentType == "" || !generated {
		return nil, nil
	}
	list := new(configsv1alpha1.AgentCredentialList)
	if err := kubeClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list compatible AgentCredentials: %w", err)
	}
	matches := make([]string, 0, len(list.Items))
	for _, credential := range list.Items {
		if string(credential.Spec.Agent) == agentType {
			matches = append(matches, credential.Name)
		}
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("multiple %s AgentCredentials exist; select an ordered credential with --credential", agentType)
	}

	return matches, nil
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

func generatedStorage(options runOptions) (*workspacesv1alpha1.PersistentStorageSpec, error) {
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

func newResumeCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "resume ID", Short: "Attach another terminal to the original live Agent Process", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			processClient, namespace, err := processClient(kubeconfigFlags)
			if err != nil {
				return err
			}
			process, err := processClient.Resolve(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}
			if process.Status.Phase != workspacesv1alpha1.AgentProcessPhaseRunning {
				return fmt.Errorf("cannot connect to AgentProcess %s in phase %s", process.Name, process.Status.Phase)
			}
			if err := processClient.Attach(cmd.Context(), process, cmd.InOrStdin(), cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("cannot connect to original AgentProcess %s: %w", process.Name, err)
			}
			return nil
		},
	}
}

func newLogsCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "logs ID", Short: "Read the persistent Agent Process transcript", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			processClient, namespace, err := processClient(kubeconfigFlags)
			if err != nil {
				return err
			}
			process, err := processClient.Resolve(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}
			return processClient.Logs(cmd.Context(), process, cmd.OutOrStdout())
		},
	}
}

func newStopCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "stop ID", Short: "Request one-way Agent Process termination", Args: cobra.ExactArgs(1),
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
			indicator := progress.Start(cmd.ErrOrStderr(), "stopping AgentProcess...")
			defer indicator.Stop()
			_, err = processClient.WaitUntilTerminal(cmd.Context(), process)
			return err
		},
	}
}

func newDeleteCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "delete ID", Short: "Delete a terminal Agent Process record and its temporary environment", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			processClient, namespace, err := processClient(kubeconfigFlags)
			if err != nil {
				return err
			}
			process, err := processClient.Resolve(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}
			if !processTerminal(process.Status.Phase) {
				return fmt.Errorf("agent process %s is still %s; stop it first", process.Name, process.Status.Phase)
			}
			return processClient.Kube.Delete(cmd.Context(), process)
		},
	}
}

func newListCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := new(listOptions)
	cmd := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List Agent Process records", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			list := new(workspacesv1alpha1.AgentProcessList)
			listOptions := []client.ListOption{}
			if !options.allNamespaces {
				listOptions = append(listOptions, client.InNamespace(namespace))
			}
			if err := clusterClient.Kube.List(cmd.Context(), list, listOptions...); err != nil {
				return err
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(writer, "NAMESPACE\tID\tTARGET\tCOMMAND\tTTY\tAGENT\tPHASE\tCLIENTS\tAGE\tEXIT"); err != nil {
				return err
			}
			for _, process := range list.Items {
				if options.workspace != "" && process.Spec.TargetRef.Name != options.workspace {
					continue
				}
				if options.phase != "" && !strings.EqualFold(string(process.Status.Phase), options.phase) {
					continue
				}
				if options.agent != "" && process.Spec.AgentType != options.agent {
					continue
				}
				if options.idPrefix != "" && !strings.HasPrefix(process.Name, options.idPrefix) {
					continue
				}
				exit := "-"
				if process.Status.ExitCode != nil {
					exit = fmt.Sprint(*process.Status.ExitCode)
				}
				age := time.Since(process.CreationTimestamp.Time).Round(time.Second)
				if process.CreationTimestamp.IsZero() {
					age = 0
				}
				command := strings.Join(process.Spec.Command, " ")
				if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%t\t%s\t%s\t%d\t%s\t%s\n", process.Namespace, process.Name, process.Spec.TargetRef.Name, command, process.Spec.TTY, process.Spec.AgentType, process.Status.Phase, process.Status.AttachedClients, age, exit); err != nil {
					return err
				}
			}
			return writer.Flush()
		},
	}
	cmd.Flags().StringVar(&options.workspace, "workspace", "", "Filter by target Workspace")
	cmd.Flags().StringVar(&options.phase, "phase", "", "Filter by process phase")
	cmd.Flags().StringVar(&options.agent, "agent", "", "Filter by recognized agent type")
	cmd.Flags().StringVar(&options.idPrefix, "id-prefix", "", "Filter by managed ID prefix")
	cmd.Flags().BoolVarP(&options.allNamespaces, "all-namespaces", "A", false, "List across all permitted namespaces")

	return cmd
}

func processClient(kubeconfigFlags *kubeconfig.Flags) (*workspaceservice.ProcessClient, string, error) {
	config, namespace, err := kubeconfigFlags.Resolve()
	if err != nil {
		return nil, "", err
	}
	clusterClient, err := cluster.New(config)
	if err != nil {
		return nil, "", err
	}

	return &workspaceservice.ProcessClient{Kube: clusterClient.Kube, Runtime: clusterClient.Processes, Config: config}, namespace, nil
}

func processTerminal(phase workspacesv1alpha1.AgentProcessPhase) bool {
	switch phase {
	case workspacesv1alpha1.AgentProcessPhaseSucceeded, workspacesv1alpha1.AgentProcessPhaseFailed,
		workspacesv1alpha1.AgentProcessPhaseStopped, workspacesv1alpha1.AgentProcessPhaseLost:
		return true
	default:
		return false
	}
}

func boolPointer(value bool) *bool {
	return &value
}
