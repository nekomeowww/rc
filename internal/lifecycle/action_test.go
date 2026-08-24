/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

func TestActionRequiresExactlyOneExecutableForm(t *testing.T) {
	t.Parallel()
	for _, action := range []Action{{}, {Command: []string{"true"}, Script: "true"}, {Command: []string{""}}} {
		err := Run(context.Background(), []Action{action}, nil, new(bytes.Buffer), new(bytes.Buffer))
		assert.Error(t, err)
	}
}
