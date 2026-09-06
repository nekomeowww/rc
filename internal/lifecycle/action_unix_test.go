//go:build !windows

package lifecycle

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActionsRoundTripAndRunInOrder(t *testing.T) {
	t.Parallel()
	actions := []Action{
		{Command: []string{"printf", "command"}, WorkingDirectory: t.TempDir()},
		{Script: "printf -- '-script'", WorkingDirectory: t.TempDir()},
	}
	encoded, err := Encode(actions)
	require.NoError(t, err)
	decoded, err := Decode(encoded)
	require.NoError(t, err)
	assert.Equal(t, actions, decoded)

	output := new(bytes.Buffer)
	require.NoError(t, Run(context.Background(), decoded, nil, output, output))
	assert.Equal(t, "command-script", output.String())
}
