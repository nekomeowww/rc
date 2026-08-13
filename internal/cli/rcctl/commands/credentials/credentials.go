package credentials

import (
	"github.com/spf13/cobra"

	"github.com/nekomeowww/rc/internal/cli/rcctl/command"
	"github.com/nekomeowww/rc/internal/cli/rcctl/commands/credentials/importcredential"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

// Register attaches credential subcommands to the root command.
func Register(root *cobra.Command, kubeconfigFlags *kubeconfig.Flags) {
	parent := NewCommand()
	command.Register(parent, func() *cobra.Command {
		return importcredential.NewCommand(kubeconfigFlags)
	})
	root.AddCommand(parent)
}
