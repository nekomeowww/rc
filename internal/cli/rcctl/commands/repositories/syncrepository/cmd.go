package syncrepository

import (
	"fmt"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	repositoryservice "github.com/nekomeowww/rc/internal/repositories"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NewCommand creates the explicit parent-mirror synchronization command.
func NewCommand(flags *kubeconfig.Flags) *cobra.Command {
	wait := true
	command := &cobra.Command{
		Use:   "sync REPOSITORY",
		Short: "Fetch and reset a repository to its configured ref",
		Long:  "Fetch the configured remote and reset the shared parent checkout, removing untracked files. Uses the Repository credential and ref. Existing Worktrees keep their contents.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, namespace, err := flags.Resolve()
			if err != nil {
				return err
			}
			scheme := runtime.NewScheme()
			if err := repositoriesv1alpha1.AddToScheme(scheme); err != nil {
				return err
			}
			kubeClient, err := client.New(config, client.Options{Scheme: scheme})
			if err != nil {
				return err
			}
			service := repositoryservice.SyncClient{Client: kubeClient}
			request, err := service.Start(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "repositorysync.repositories.rc.ayaka.io/%s created\n", request.Name); err != nil {
				return err
			}
			if !wait {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), request.Name)
				return err
			}
			result, err := service.Wait(cmd.Context(), request)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", args[0], result.Status.Commit)
			return err
		},
	}
	command.Flags().BoolVar(&wait, "wait", true, "Wait for this sync request and print the resolved commit")
	return command
}
