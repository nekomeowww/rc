package repositories

import (
	"github.com/spf13/cobra"

	"github.com/nekomeowww/rc/internal/cli/rcctl/command"
)

// NewCommand creates the Repository command group.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "repo",
		Aliases: []string{"repository"},
		Short:   "Manage repositories",
		GroupID: command.RepositoriesGroup,
	}
}
