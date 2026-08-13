package command

import "github.com/spf13/cobra"

const CredentialsGroup = "credentials"

// Register attaches command constructors to their direct parent.
func Register(parent *cobra.Command, constructors ...func() *cobra.Command) {
	for _, constructor := range constructors {
		parent.AddCommand(constructor())
	}
}
