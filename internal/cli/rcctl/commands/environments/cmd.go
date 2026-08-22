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

package environments

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/cli/rcctl/cluster"
	"github.com/nekomeowww/rc/internal/cli/rcctl/command"
	"github.com/nekomeowww/rc/internal/cli/rcctl/progress"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	workspaceservice "github.com/nekomeowww/rc/internal/workspaces"
)

type createOptions struct {
	image        string
	storageClass string
	size         string
	idleTimeout  time.Duration
	wait         bool
}

type processOptions struct {
	agentCredential  string
	credentials      []string
	environment      []string
	environmentFiles []string
	noPassthrough    bool
	cwd              string
}

// Register attaches Workspace Environment commands to rcctl.
func Register(root *cobra.Command, kubeconfigFlags *kubeconfig.Flags) {
	root.AddCommand(NewCommand(kubeconfigFlags))
}

func NewCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	root := &cobra.Command{Use: "env", Aliases: []string{"environment", "environments"}, Short: "Manage reusable Workspace Environments", GroupID: command.EnvironmentsGroup}
	root.AddCommand(newCreateCommand(kubeconfigFlags), newEditCommand(kubeconfigFlags), newExecCommand(kubeconfigFlags), newCommitCommand(kubeconfigFlags), newStopCommand(kubeconfigFlags), newDeleteCommand(kubeconfigFlags), newDefaultCommand(kubeconfigFlags))

	return root
}

func newCreateCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := new(createOptions)
	cmd := &cobra.Command{
		Use: "create NAME", Short: "Create a reusable Environment current volume", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			size, err := resource.ParseQuantity(options.size)
			if err != nil || size.Sign() <= 0 {
				return fmt.Errorf("parse --size: value must be a positive Kubernetes quantity")
			}
			environment := &workspacesv1alpha1.WorkspaceEnvironment{
				ObjectMeta: metav1.ObjectMeta{Name: args[0], Namespace: namespace},
				Spec: workspacesv1alpha1.WorkspaceEnvironmentSpec{
					Image:   options.image,
					Storage: workspacesv1alpha1.PersistentStorageSpec{StorageClassName: options.storageClass, Size: size},
				},
			}
			if options.idleTimeout >= 0 {
				environment.Spec.EditorIdleTimeout = &metav1.Duration{Duration: options.idleTimeout}
			}
			if err := clusterClient.Kube.Create(cmd.Context(), environment); err != nil {
				return fmt.Errorf("create WorkspaceEnvironment: %w", err)
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "workspaceenvironment.workspaces.rc.ayaka.io/%s created\n", environment.Name); err != nil {
				return err
			}
			if !options.wait {
				return nil
			}

			indicator := progress.Start(cmd.ErrOrStderr(), "preparing Environment...")
			defer indicator.Stop()
			return waitEnvironmentReady(cmd.Context(), clusterClient.Kube, environment)
		},
	}
	cmd.Flags().StringVar(&options.image, "image", "", "Workspace runner image")
	cmd.Flags().StringVar(&options.storageClass, "storage-class", "", "Clone-capable StorageClass")
	cmd.Flags().StringVar(&options.size, "size", "20Gi", "Environment home volume size")
	cmd.Flags().DurationVar(&options.idleTimeout, "editor-idle-timeout", 10*time.Minute, "Idle editor suspension timeout; zero disables")
	cmd.Flags().BoolVar(&options.wait, "wait", true, "Wait for current Environment revision")
	_ = cmd.MarkFlagRequired("image")
	_ = cmd.MarkFlagRequired("storage-class")

	return cmd
}

func newEditCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := new(processOptions)
	cmd := &cobra.Command{
		Use: "edit [flags] NAME [--] [SHELL [ARG...]]", Short: "Open an interactive shell against Environment draft", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			processCommand := commandAfterEnvironmentName(args)
			if len(processCommand) == 0 {
				processCommand = []string{"/bin/bash"}
			}
			return runEnvironmentProcess(cmd, kubeconfigFlags, args[0], processCommand, true, *options)
		},
	}
	addProcessFlags(cmd, options)

	return cmd
}

func newExecCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := new(processOptions)
	cmd := &cobra.Command{
		Use: "exec [flags] NAME [--] COMMAND [ARG...]", Short: "Run a non-terminal command against Environment draft", Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			processCommand := commandAfterEnvironmentName(args)
			if len(processCommand) == 0 {
				return fmt.Errorf("command is required after workspace environment name")
			}
			return runEnvironmentProcess(cmd, kubeconfigFlags, args[0], processCommand, false, *options)
		},
	}
	addProcessFlags(cmd, options)

	return cmd
}

func commandAfterEnvironmentName(args []string) []string {
	argv := args[1:]
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}

	return argv
}

func addProcessFlags(cmd *cobra.Command, options *processOptions) {
	cmd.Flags().StringVar(&options.agentCredential, "credential", "", "AgentCredential to expose for this edit operation")
	cmd.Flags().StringArrayVar(&options.credentials, "dangerously-include-credentials", nil, "Generic Credential to expose; repeat by name")
	cmd.Flags().StringArrayVar(&options.environment, "env", nil, "Explicit NAME or NAME=value; repeat")
	cmd.Flags().StringArrayVar(&options.environmentFiles, "env-file", nil, "Read environment values from a file; repeat")
	cmd.Flags().BoolVar(&options.noPassthrough, "no-env-passthrough", false, "Disable caller environment pass-through")
	cmd.Flags().StringVar(&options.cwd, "cwd", "", "Working directory")
}

func runEnvironmentProcess(cmd *cobra.Command, kubeconfigFlags *kubeconfig.Flags, name string, processCommand []string, tty bool, options processOptions) error {
	config, namespace, err := kubeconfigFlags.Resolve()
	if err != nil {
		return err
	}
	clusterClient, err := cluster.New(config)
	if err != nil {
		return err
	}
	environment := new(workspacesv1alpha1.WorkspaceEnvironment)
	if err := clusterClient.Kube.Get(cmd.Context(), client.ObjectKey{Name: name, Namespace: namespace}, environment); err != nil {
		return fmt.Errorf("get WorkspaceEnvironment: %w", err)
	}
	if !meta.IsStatusConditionTrue(environment.Status.Conditions, workspacesv1alpha1.WorkspaceEnvironmentConditionReady) {
		return fmt.Errorf("workspace environment %q is not Ready", name)
	}
	files, err := workspaceservice.ReadEnvironmentFiles(options.environmentFiles)
	if err != nil {
		return err
	}
	values, err := workspaceservice.BuildProcessEnvironment(workspaceservice.EnvironmentOptions{
		Caller: os.Environ(), NoPassthrough: options.noPassthrough, Files: files, Explicit: options.environment, Lookup: os.LookupEnv,
	})
	if err != nil {
		return err
	}
	agentType := workspaceservice.AgentTypeForCommand(processCommand[0])
	processClient := &workspaceservice.ProcessClient{Kube: clusterClient.Kube, Runtime: clusterClient.Processes, Config: clusterClient.Config}
	process, err := processClient.Start(cmd.Context(), workspaceservice.ProcessStartRequest{
		Namespace: namespace,
		Target:    workspacesv1alpha1.AgentProcessTargetReference{Kind: workspacesv1alpha1.AgentProcessTargetWorkspaceEnvironment, Name: name},
		Command:   processCommand, WorkingDirectory: options.cwd, TTY: tty, AgentType: agentType,
		AgentCredential: options.agentCredential, Credentials: options.credentials, Environment: values,
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
	if terminal(ready.Status.Phase) {
		if err := processClient.Logs(cmd.Context(), ready, cmd.OutOrStdout()); err != nil {
			return err
		}
	} else {
		if err := processClient.Attach(cmd.Context(), ready, cmd.InOrStdin(), cmd.OutOrStdout()); err != nil && cmd.Context().Err() == nil {
			return err
		}
	}
	finished := ready
	if !terminal(ready.Status.Phase) {
		finished, err = processClient.WaitUntilTerminal(cmd.Context(), process)
		if err != nil {
			return err
		}
	}
	return workspaceservice.ResultError(finished)
}

func newCommitCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "commit NAME", Short: "Promote Environment draft to current", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			environment := new(workspacesv1alpha1.WorkspaceEnvironment)
			key := client.ObjectKey{Name: args[0], Namespace: namespace}
			if err := clusterClient.Kube.Get(cmd.Context(), key, environment); err != nil {
				return fmt.Errorf("get WorkspaceEnvironment: %w", err)
			}
			environment.Spec.Commit++
			requested := environment.Spec.Commit
			if err := clusterClient.Kube.Update(cmd.Context(), environment); err != nil {
				return fmt.Errorf("request Environment commit: %w", err)
			}

			indicator := progress.Start(cmd.ErrOrStderr(), "committing Environment...")
			defer indicator.Stop()
			return wait.PollUntilContextCancel(cmd.Context(), 300*time.Millisecond, true, func(ctx context.Context) (bool, error) {
				current := new(workspacesv1alpha1.WorkspaceEnvironment)
				if err := clusterClient.Kube.Get(ctx, key, current); err != nil {
					return false, err
				}
				if current.Status.CommittedRequest >= requested {
					return true, nil
				}
				condition := meta.FindStatusCondition(current.Status.Conditions, workspacesv1alpha1.WorkspaceEnvironmentConditionDraftReady)
				if condition != nil && condition.ObservedGeneration == current.Generation {
					switch condition.Reason {
					case "NoDraft", "ActiveProcesses", "DraftMissing":
						return false, fmt.Errorf("environment commit rejected: %s", condition.Message)
					}
				}
				return false, nil
			})
		},
	}
}

func newStopCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "stop NAME", Short: "Stop an idle Environment editor without deleting draft", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			processes := new(workspacesv1alpha1.AgentProcessList)
			if err := clusterClient.Kube.List(cmd.Context(), processes, client.InNamespace(namespace)); err != nil {
				return err
			}
			for index := range processes.Items {
				process := &processes.Items[index]
				if process.Spec.TargetRef.Kind == workspacesv1alpha1.AgentProcessTargetWorkspaceEnvironment && process.Spec.TargetRef.Name == args[0] && !terminal(process.Status.Phase) {
					return fmt.Errorf("workspace environment %q has active AgentProcess %q", args[0], process.Name)
				}
			}
			environment := new(workspacesv1alpha1.WorkspaceEnvironment)
			if err := clusterClient.Kube.Get(cmd.Context(), client.ObjectKey{Name: args[0], Namespace: namespace}, environment); err != nil {
				return err
			}
			if environment.Status.EditorPodName == "" {
				return nil
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: environment.Status.EditorPodName, Namespace: namespace}}
			if err := clusterClient.Kube.Delete(cmd.Context(), pod); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("delete Environment editor Pod: %w", err)
			}
			return nil
		},
	}
}

func newDeleteCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "delete NAME", Short: "Delete a WorkspaceEnvironment and its current and draft volumes", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			clusterClient, err := cluster.New(config)
			if err != nil {
				return err
			}
			environment := &workspacesv1alpha1.WorkspaceEnvironment{ObjectMeta: metav1.ObjectMeta{Name: args[0], Namespace: namespace}}
			return clusterClient.Kube.Delete(cmd.Context(), environment)
		},
	}
}

func newDefaultCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "default NAME", Short: "Set the XDG default Environment for the current context and namespace", Args: cobra.ExactArgs(1),
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
			defaults.Environment = args[0]
			return store.Set(contextName, namespace, defaults)
		},
	}
}

func waitEnvironmentReady(ctx context.Context, kubeClient client.Client, environment *workspacesv1alpha1.WorkspaceEnvironment) error {
	return wait.PollUntilContextCancel(ctx, 300*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current := new(workspacesv1alpha1.WorkspaceEnvironment)
		if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(environment), current); err != nil {
			return false, err
		}
		condition := meta.FindStatusCondition(current.Status.Conditions, workspacesv1alpha1.WorkspaceEnvironmentConditionReady)
		return condition != nil && condition.Status == metav1.ConditionTrue, nil
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
