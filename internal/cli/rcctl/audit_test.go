package rcctl

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaintenanceCommandsRegistered(t *testing.T) {
	for _, name := range []string{"doctor", "prune"} {
		command := NewCommand()
		output := new(bytes.Buffer)
		command.SetOut(output)
		command.SetArgs([]string{name, "--help"})
		require.NoError(t, command.Execute())
		assert.Contains(t, output.String(), name)
	}
}
