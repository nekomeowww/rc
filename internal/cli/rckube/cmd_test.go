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

package rckube

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nekomeowww/rc/internal/lifecycle"
)

func TestLifecycleCommandRunsEncodedActions(t *testing.T) {
	t.Parallel()
	encoded, err := lifecycle.Encode([]lifecycle.Action{{
		Command: []string{"printf", "initialized"}, WorkingDirectory: t.TempDir(),
	}})
	require.NoError(t, err)
	output := new(bytes.Buffer)
	command := NewCommand()
	command.SetArgs([]string{"lifecycle", "--actions", encoded})
	command.SetOut(output)
	command.SetErr(output)

	require.NoError(t, command.ExecuteContext(context.Background()))
	assert.Equal(t, "initialized", output.String())
}
