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
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/lifecycle"
)

func TestLifecycleCommandRunsEncodedActions(t *testing.T) {
	t.Parallel()
	argv := []string{"printf", "initialized"}
	if runtime.GOOS == "windows" {
		argv = []string{"powershell.exe", "-NoProfile", "-Command", "[Console]::Write('initialized')"}
	}
	encoded, err := lifecycle.Encode([]lifecycle.Action{{
		Command: argv, WorkingDirectory: t.TempDir(),
	}})
	require.NoError(t, err)
	output := new(bytes.Buffer)
	command := NewCommand()
	command.SetArgs([]string{lifecycleCommandName, "--actions", encoded})
	command.SetOut(output)
	command.SetErr(output)

	require.NoError(t, command.ExecuteContext(context.Background()))
	assert.Equal(t, "initialized", output.String())
}

func TestLifecycleCommandHidesKubernetesEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the flag is set only on Linux runtime Pods")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("RC_TEST_ORDINARY", "kept")
	encoded, err := lifecycle.Encode([]lifecycle.Action{{
		Command:          []string{"env"},
		WorkingDirectory: t.TempDir(),
	}})
	require.NoError(t, err)
	output := new(bytes.Buffer)
	command := NewCommand()
	command.SetArgs([]string{lifecycleCommandName, "--" + processruntime.HideKubernetesEnvironmentFlag, "--actions", encoded})
	command.SetOut(output)
	command.SetErr(output)

	require.NoError(t, command.ExecuteContext(context.Background()))
	assert.NotContains(t, output.String(), "KUBERNETES_", "a lifecycle action does not inherit Kubernetes variables")
	assert.Contains(t, output.String(), "RC_TEST_ORDINARY=kept", "a lifecycle action inherits other variables")
}
