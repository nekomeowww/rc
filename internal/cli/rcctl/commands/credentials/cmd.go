package credentials

import (
	"github.com/spf13/cobra"

	"github.com/nekomeowww/rc/internal/cli/rcctl/command"
)

// NewCommand creates the credential command group.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "credentials",
		Short:   "Manage credentials",
		GroupID: command.CredentialsGroup,
	}
}
