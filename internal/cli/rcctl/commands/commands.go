package commands

import (
	"github.com/spf13/cobra"

	"github.com/nekomeowww/rc/internal/cli/rcctl/commands/credentials"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

// Register attaches every top-level rcctl command.
func Register(root *cobra.Command, kubeconfigFlags *kubeconfig.Flags) {
	credentials.Register(root, kubeconfigFlags)
}
