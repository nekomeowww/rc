package rcctl

import (
	"github.com/spf13/cobra"

	"github.com/nekomeowww/rc/internal/cli/rcctl/command"
	"github.com/nekomeowww/rc/internal/cli/rcctl/commands"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

// NewCommand builds the rcctl command tree without connecting to Kubernetes.
func NewCommand() *cobra.Command {
	kubeconfigFlags := kubeconfig.NewFlags()
	root := &cobra.Command{
		Use:           "rcctl",
		Short:         "Manage rc resources",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.AddGroup(&cobra.Group{ID: command.CredentialsGroup, Title: "Credential Commands:"})
	root.AddGroup(&cobra.Group{ID: command.RepositoriesGroup, Title: "Repository Commands:"})
	root.AddGroup(&cobra.Group{ID: command.WorktreesGroup, Title: "Worktree Commands:"})
	root.AddGroup(&cobra.Group{ID: command.EnvironmentsGroup, Title: "Environment Commands:"})
	root.AddGroup(&cobra.Group{ID: command.WorkspacesGroup, Title: "Workspace Commands:"})
	root.AddGroup(&cobra.Group{ID: command.AgentsGroup, Title: "Agent Process Commands:"})
	kubeconfigFlags.AddFlags(root.PersistentFlags())
	commands.Register(root, kubeconfigFlags)
	return root
}

// New returns a command ready for execution.
func New() (*cobra.Command, error) {
	return NewCommand(), nil
}
