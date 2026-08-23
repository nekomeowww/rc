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

package agentprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testProcessID = "codex-01"

type recordingPodExecutor struct {
	target  Target
	command []string
	input   []byte
	output  State
}

func (executor *recordingPodExecutor) Exec(_ context.Context, target Target, command []string, input io.Reader, output io.Writer, _ io.Writer, _ bool) error {
	executor.target = target
	executor.command = append([]string(nil), command...)
	if input != nil {
		executor.input, _ = io.ReadAll(input)
	}
	return json.NewEncoder(output).Encode(executor.output)
}

func TestKubeRuntimeStartsThroughRcKubeBridge(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	executor := &recordingPodExecutor{output: State{ID: testProcessID, UID: "uid-01", Phase: "Running", PID: 17}}
	runtime := NewKubeRuntime(executor)
	target := Target{Namespace: "development", Pod: "coding", Container: runtimeBridgeCommand}
	request := StartRequest{ID: testProcessID, UID: "uid-01", Command: []string{"codex", "task"}}

	state, err := runtime.Start(context.Background(), target, request)
	requirements.NoError(err, "start through Pod exec bridge")
	expectedRequest, err := json.Marshal(request)
	requirements.NoError(err, "marshal expected request")
	assertions.Equal(executor.output, state, "decode supervisor state")
	assertions.Equal(target, executor.target, "exec in selected runtime")
	assertions.Equal([]string{runtimeBridgeCommand, runtimeProcessGroup, "start", runtimeSocketFlag, DefaultSocketPath}, executor.command, "invoke versioned bridge command")
	assertions.JSONEq(string(expectedRequest), string(bytes.TrimSpace(executor.input)), "send exact start request")
}

func TestKubeRuntimeAttachesWithInitialTerminalSize(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	executor := &recordingPodExecutor{}
	runtime := NewKubeRuntime(executor)
	target := Target{Namespace: "development", Pod: "coding", Container: runtimeBridgeCommand}

	err := runtime.Attach(context.Background(), target, testProcessID, "terminal-01", nil, io.Discard, true, 42, 120)
	requirements.NoError(err, "attach through Pod exec bridge")
	assertions.Equal(target, executor.target, "exec in selected runtime")
	assertions.Equal(
		[]string{runtimeBridgeCommand, runtimeProcessGroup, "attach", testProcessID, runtimeSocketFlag, DefaultSocketPath, runtimeClientIDFlag, "terminal-01", "--rows", "42", "--columns", "120"},
		executor.command,
		"send the initial terminal size with the attach request",
	)
}
