package rcctl

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helpArgument = "--help"

func TestCommandTreeContainsAgentCredentialImport(t *testing.T) {
	t.Parallel()

	command := NewCommand()
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{"credentials", "import", helpArgument})

	require.NoError(t, command.Execute(), "render import command help")
	assert.Contains(t, output.String(), "--type string")
	assert.Contains(t, output.String(), "--agent string")
	assert.Contains(t, output.String(), "--file string")
	assert.Contains(t, output.String(), "--hostname string")
	assert.Contains(t, output.String(), "--name string")
	assert.Contains(t, output.String(), "--kubeconfig string")
	assert.Contains(t, output.String(), "--context string")
	assert.Contains(t, output.String(), "--namespace string")
}

func TestCommandTreeContainsRepositoryExec(t *testing.T) {
	t.Parallel()

	command := NewCommand()
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{"repo", "exec", helpArgument})

	require.NoError(t, command.Execute(), "render Repository Exec command help")
	assert.Contains(t, output.String(), "exec REPOSITORY -- COMMAND [ARG...]")
	assert.Contains(t, output.String(), "--wait")
}

func TestCommandTreeContainsRepositoryClone(t *testing.T) {
	t.Parallel()

	command := NewCommand()
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{"repo", "clone", helpArgument})

	require.NoError(t, command.Execute(), "render Repository Clone command help")
	assert.Contains(t, output.String(), "clone URL")
	assert.Contains(t, output.String(), "--storage-class string")
	assert.Contains(t, output.String(), "--size string")
	assert.Contains(t, output.String(), "--ref string")
	assert.Contains(t, output.String(), "--credential-ref string")
	assert.Contains(t, output.String(), "--name string")
}

func TestCommandTreeContainsWorkspaceRuntimeCommands(t *testing.T) {
	t.Parallel()
	command := NewCommand()
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{helpArgument})

	require.NoError(t, command.Execute(), "render root help")
	assert.Contains(t, output.String(), "env")
	assert.Contains(t, output.String(), "workspace")
	assert.Contains(t, output.String(), "agent")
}
