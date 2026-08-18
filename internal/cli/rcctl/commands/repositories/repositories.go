package repositories

import (
	"github.com/spf13/cobra"

	"github.com/nekomeowww/rc/internal/cli/rcctl/commands/repositories/execrepository"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

// Register attaches Repository commands to the root command.
func Register(root *cobra.Command, kubeconfigFlags *kubeconfig.Flags) {
	command := NewCommand()
	command.AddCommand(execrepository.NewCommand(kubeconfigFlags))
	root.AddCommand(command)
}
