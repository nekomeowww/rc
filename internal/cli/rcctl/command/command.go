package command

import "github.com/spf13/cobra"

const CredentialsGroup = "credentials"

// RepositoriesGroup contains commands that manage Repository resources.
const RepositoriesGroup = "repositories"

// WorktreesGroup contains commands that manage Worktree resources.
const WorktreesGroup = "worktrees"

// Register attaches command constructors to their direct parent.
func Register(parent *cobra.Command, constructors ...func() *cobra.Command) {
	for _, constructor := range constructors {
		parent.AddCommand(constructor())
	}
}
