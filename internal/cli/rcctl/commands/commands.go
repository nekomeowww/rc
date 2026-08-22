package commands

import (
	"github.com/spf13/cobra"

	"github.com/nekomeowww/rc/internal/cli/rcctl/commands/agents"
	"github.com/nekomeowww/rc/internal/cli/rcctl/commands/credentials"
	"github.com/nekomeowww/rc/internal/cli/rcctl/commands/environments"
	"github.com/nekomeowww/rc/internal/cli/rcctl/commands/repositories"
	workspacecommands "github.com/nekomeowww/rc/internal/cli/rcctl/commands/workspaces"
	"github.com/nekomeowww/rc/internal/cli/rcctl/commands/worktrees"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

// Register attaches every top-level rcctl command.
func Register(root *cobra.Command, kubeconfigFlags *kubeconfig.Flags) {
	credentials.Register(root, kubeconfigFlags)
	repositories.Register(root, kubeconfigFlags)
	worktrees.Register(root, kubeconfigFlags)
	environments.Register(root, kubeconfigFlags)
	workspacecommands.Register(root, kubeconfigFlags)
	agents.Register(root, kubeconfigFlags)
}
