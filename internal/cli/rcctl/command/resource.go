/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package command

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nekomeowww/rc/internal/cli/rcctl/cluster"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	workspaceservice "github.com/nekomeowww/rc/internal/workspaces"
	clioutput "github.com/nekomeowww/rc/pkg/output"
)

// ListFunc lists resources in namespace and prints them with options.
type ListFunc func(ctx context.Context, writer io.Writer, kubeClient client.Client, namespace string, options clioutput.Options) error

// NewListCommand creates a `list` command that prints resources in the
// current namespace through run.
func NewListCommand(kubeconfigFlags *kubeconfig.Flags, short string, run ListFunc) *cobra.Command {
	options := new(clioutput.Options)
	cmd := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: short, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := options.Validate(true); err != nil {
				return err
			}
			clusterClient, namespace, err := cluster.Connect(kubeconfigFlags)
			if err != nil {
				return err
			}

			return run(cmd.Context(), cmd.OutOrStdout(), clusterClient.Kube, namespace, *options)
		},
	}
	options.AddFlags(cmd, true)

	return cmd
}

// NewGetCommand creates a `get NAME` command that prints the details of one
// namespaced kind resource.
func NewGetCommand[T client.Object](
	kubeconfigFlags *kubeconfig.Flags,
	kind string,
	newObject func() T,
	fields func(T) []clioutput.Field,
) *cobra.Command {
	options := new(clioutput.Options)
	cmd := &cobra.Command{
		Use: "get NAME", Short: "Show a " + kind, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := options.Validate(false); err != nil {
				return err
			}
			clusterClient, namespace, err := cluster.Connect(kubeconfigFlags)
			if err != nil {
				return err
			}
			object := newObject()
			if err := clusterClient.Kube.Get(cmd.Context(), client.ObjectKey{Namespace: namespace, Name: args[0]}, object); err != nil {
				return fmt.Errorf("get %s %q: %w", kind, args[0], err)
			}
			return options.PrintDetails(cmd.OutOrStdout(), object, clusterClient.Kube.Scheme(), fields(object))
		},
	}
	options.AddFlags(cmd, false)

	return cmd
}

// NewDefaultCommand creates a `default NAME` command that stores NAME in the
// XDG defaults for the current context and namespace through set.
func NewDefaultCommand(kubeconfigFlags *kubeconfig.Flags, short string, set func(*workspaceservice.Defaults, string)) *cobra.Command {
	return &cobra.Command{
		Use: "default NAME", Short: short, Args: cobra.ExactArgs(1),
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
			set(&defaults, args[0])
			return store.Set(contextName, namespace, defaults)
		},
	}
}
