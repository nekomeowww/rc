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

package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

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

	err := runtime.Attach(context.Background(), target, testProcessID, "terminal-01", nil, io.Discard, io.Discard, AttachOptions{Rows: 42, Columns: 120})
	requirements.NoError(err, "attach through Pod exec bridge")
	assertions.Equal(target, executor.target, "exec in selected runtime")
	assertions.Equal(
		[]string{runtimeBridgeCommand, runtimeProcessGroup, "attach", testProcessID, runtimeSocketFlag, DefaultSocketPath, runtimeClientIDFlag, "terminal-01", "--rows", "42", "--columns", "120"},
		executor.command,
		"send the initial terminal size with the attach request",
	)
}

func TestKubeRuntimeUsesWindowsTargetFromAnyHost(t *testing.T) {
	t.Parallel()
	executor := &recordingPodExecutor{output: State{ID: testProcessID, Phase: "Running"}}
	runtime := NewKubeRuntime(executor)
	target := Target{Namespace: "windows-test", Pod: "windows", Container: runtimeBridgeCommand, Executable: "rc-kube.exe", Endpoint: `\\.\pipe\rc-kube`}
	_, err := runtime.Start(context.Background(), target, StartRequest{ID: testProcessID, Command: []string{"electron.exe", "main.cjs"}})
	require.NoError(t, err)
	require.Len(t, executor.command, 5)
	assert.Equal(t, "rc-kube.exe", executor.command[0])
	assert.Equal(t, `\\.\pipe\rc-kube`, executor.command[4])
	err = runtime.Attach(context.Background(), target, testProcessID, "terminal-01", nil, io.Discard, io.Discard, AttachOptions{Rows: 24, Columns: 80})
	require.NoError(t, err)
	require.Len(t, executor.command, 12)
	assert.Equal(t, "rc-kube.exe", executor.command[0])
	assert.Equal(t, `\\.\pipe\rc-kube`, executor.command[5])
}

type historyPodExecutor struct {
	command []string
	entries []HistoryEntry
}

func (executor *historyPodExecutor) Exec(_ context.Context, _ Target, command []string, _ io.Reader, output io.Writer, _ io.Writer, _ bool) error {
	executor.command = append([]string(nil), command...)
	return json.NewEncoder(output).Encode(executor.entries)
}

func TestKubeRuntimeListsHistoryThroughRcKubeBridge(t *testing.T) {
	t.Parallel()
	executor := &historyPodExecutor{entries: []HistoryEntry{{State: State{ID: testProcessID, Phase: "Exited"}, Command: []string{"pnpm", "test"}}}}
	entries, err := NewKubeRuntime(executor).List(t.Context(), Target{})
	require.NoError(t, err)
	require.Equal(t, executor.entries, entries)
	require.Equal(t, []string{runtimeBridgeCommand, runtimeProcessGroup, "list", runtimeSocketFlag, DefaultSocketPath}, executor.command)
}

func TestKubeRuntimePrunesHistoryWithPolicy(t *testing.T) {
	t.Parallel()
	executor := &recordingPodExecutor{}
	policy := HistoryPolicy{TTLAfterFinished: 2160 * time.Hour, MaxEntries: 15000, TranscriptTTL: 336 * time.Hour}
	require.NoError(t, NewKubeRuntime(executor).PruneHistory(t.Context(), Target{}, policy))
	require.Equal(t, []string{
		runtimeBridgeCommand, runtimeProcessGroup, "prune-history",
		"--ttl-after-finished", "2160h0m0s",
		"--max-entries", "15000",
		"--transcript-ttl", "336h0m0s",
		runtimeSocketFlag, DefaultSocketPath,
	}, executor.command)
}

func TestKubeRuntimeCopiesThroughRcKube(t *testing.T) {
	t.Parallel()
	executor := &recordingPodExecutor{}
	runtime := NewKubeRuntime(executor)
	target := Target{Executable: "copy-bridge"}
	require.NoError(t, runtime.CopyTo(t.Context(), target, "artifacts", bytes.NewReader([]byte("tar"))))
	require.Equal(t, []string{"copy-bridge", "copy-to", "artifacts"}, executor.command)
	require.Equal(t, []byte("tar"), executor.input, "stream the archive on standard input")
	require.NoError(t, runtime.CopyFrom(t.Context(), Target{}, "/home/agent/out", io.Discard))
	require.Equal(t, []string{runtimeBridgeCommand, "copy-from", "/home/agent/out"}, executor.command)
}
