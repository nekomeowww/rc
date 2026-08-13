package rcctl

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandTreeContainsAgentCredentialImport(t *testing.T) {
	t.Parallel()

	command := NewCommand()
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{"credentials", "import", "--help"})

	require.NoError(t, command.Execute(), "render import command help")
	assert.Contains(t, output.String(), "--type string")
	assert.Contains(t, output.String(), "--agent string")
	assert.Contains(t, output.String(), "--file string")
	assert.Contains(t, output.String(), "--kubeconfig string")
	assert.Contains(t, output.String(), "--context string")
	assert.Contains(t, output.String(), "--namespace string")
}
