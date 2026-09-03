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

package worktrees

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	repositoryservice "github.com/nekomeowww/rc/internal/repositories"
)

func newExecCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	var waitForCompletion bool
	cmd := &cobra.Command{
		Use: "exec WORKTREE -- COMMAND [ARG...]", Short: "Execute an exact command in an isolated Worktree Job", Args: exactCommandArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			scheme := runtime.NewScheme()
			if err := repositoriesv1alpha1.AddToScheme(scheme); err != nil {
				return fmt.Errorf("register Repository API types: %w", err)
			}
			kubeClient, err := client.New(config, client.Options{Scheme: scheme})
			if err != nil {
				return fmt.Errorf("create Kubernetes client: %w", err)
			}
			clientset, err := kubernetes.NewForConfig(config)
			if err != nil {
				return fmt.Errorf("create Kubernetes clientset: %w", err)
			}
			execClient := &repositoryservice.WorktreeExecClient{Client: kubeClient, Kubernetes: clientset}
			exec, err := execClient.Start(cmd.Context(), repositoryservice.WorktreeExecRequest{Namespace: namespace, Worktree: args[0], Command: args[1:]})
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "worktreeexec.repositories.rc.ayaka.io/%s created\n", exec.Name); err != nil {
				return err
			}
			if !waitForCompletion {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), exec.Name)
				return err
			}
			return execClient.Wait(cmd.Context(), exec, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&waitForCompletion, "wait", true, "Wait for completion and write command output")
	return cmd
}

func exactCommandArgs(cmd *cobra.Command, args []string) error {
	if cmd.ArgsLenAtDash() != 1 {
		return errors.New("expected exactly one Worktree name before --")
	}
	if len(args) < 2 {
		return errors.New("expected a command after --")
	}
	return nil
}
