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
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const DefaultSocketPath = "/run/rc/rc-kube.sock"

const (
	runtimeBridgeCommand = "rc-kube"
	runtimeProcessGroup  = "process"
	runtimeSocketFlag    = "--socket"
	runtimeClientIDFlag  = "--client-id"
)

// PodExecutor is the Kubernetes Pod exec system boundary.
type PodExecutor interface {
	Exec(context.Context, Target, []string, io.Reader, io.Writer, io.Writer, bool) error
}

// KubeRuntime reaches rc-kube through a short-lived Pod exec bridge.
type KubeRuntime struct {
	executor PodExecutor
}

func NewKubeRuntime(executor PodExecutor) *KubeRuntime {
	return &KubeRuntime{executor: executor}
}

func (runtime *KubeRuntime) Start(ctx context.Context, target Target, request StartRequest) (State, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return State{}, fmt.Errorf("encode process start request: %w", err)
	}

	return runtime.stateCommand(ctx, target, bridgeCommand(target, "start"), bytes.NewReader(data))
}

func (runtime *KubeRuntime) Inspect(ctx context.Context, target Target, id string) (State, error) {
	return runtime.stateCommand(ctx, target, bridgeCommand(target, "inspect", id), nil)
}

func (runtime *KubeRuntime) Stop(ctx context.Context, target Target, id string) (State, error) {
	return runtime.stateCommand(ctx, target, bridgeCommand(target, "stop", id), nil)
}

// Attach connects input and output to a process. Process output arrives on
// output; errors of the bridge itself arrive on errorOutput.
func (runtime *KubeRuntime) Attach(ctx context.Context, target Target, id string, clientID string, input io.Reader, output, errorOutput io.Writer, options AttachOptions) error {
	// The child owns the PTY. The Kubernetes exec bridge stays a transparent
	// byte stream so it does not introduce a second terminal line discipline.
	command := append(bridgeCommand(target, "attach", id), runtimeClientIDFlag, clientID)
	if options.Rows > 0 && options.Columns > 0 {
		command = append(command, "--rows", fmt.Sprint(options.Rows), "--columns", fmt.Sprint(options.Columns))
	}
	if options.Lossless {
		command = append(command, "--lossless")
	}
	if options.CloseInputOnEOF {
		command = append(command, "--close-input-on-eof")
	}
	return runtime.executor.Exec(ctx, target, command, input, output, errorOutput, false)
}

// Logs streams a process transcript to output. Errors of the bridge itself
// arrive on errorOutput.
func (runtime *KubeRuntime) Logs(ctx context.Context, target Target, id string, output, errorOutput io.Writer) error {
	return runtime.executor.Exec(ctx, target, bridgeCommand(target, "logs", id), nil, output, errorOutput, false)
}

func (runtime *KubeRuntime) Resize(ctx context.Context, target Target, id string, clientID string, rows uint16, columns uint16) error {
	command := append(bridgeCommand(target, "resize", id), runtimeClientIDFlag, clientID, "--rows", fmt.Sprint(rows), "--columns", fmt.Sprint(columns))
	return runtime.executor.Exec(ctx, target, command, nil, io.Discard, io.Discard, false)
}

func bridgeCommand(target Target, arguments ...string) []string {
	executable := target.Executable
	if executable == "" {
		executable = runtimeBridgeCommand
	}
	endpoint := target.Endpoint
	if endpoint == "" {
		endpoint = DefaultSocketPath
	}
	command := []string{executable, runtimeProcessGroup}
	command = append(command, arguments...)
	return append(command, runtimeSocketFlag, endpoint)
}

func (runtime *KubeRuntime) stateCommand(ctx context.Context, target Target, command []string, input io.Reader) (State, error) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	if err := runtime.executor.Exec(ctx, target, command, input, stdout, stderr, false); err != nil {
		message := strings.TrimSpace(stderr.String())
		if strings.Contains(message, ErrNotFound.Error()) {
			return State{}, fmt.Errorf("%w: %s", ErrNotFound, message)
		}
		if message != "" {
			return State{}, fmt.Errorf("exec rc-kube bridge: %w: %s", err, message)
		}
		return State{}, fmt.Errorf("exec rc-kube bridge: %w", err)
	}
	state := State{}
	if err := json.NewDecoder(stdout).Decode(&state); err != nil {
		if errors.Is(err, io.EOF) {
			return State{}, errors.New("rc-kube bridge returned no process state")
		}
		return State{}, fmt.Errorf("decode rc-kube process state: %w", err)
	}

	return state, nil
}

// PruneHistory applies a retention policy to the target supervisor's finished
// process records.
func (runtime *KubeRuntime) PruneHistory(ctx context.Context, target Target, policy HistoryPolicy) error {
	command := bridgeCommand(target, "prune-history",
		"--ttl-after-finished", policy.TTLAfterFinished.String(),
		"--max-entries", strconv.Itoa(policy.MaxEntries),
		"--transcript-ttl", policy.TranscriptTTL.String(),
	)
	_, err := runtime.stateCommand(ctx, target, command, nil)
	return err
}

// CopyFrom streams path in the target runtime to output as a tar stream.
// A relative path is resolved against the runtime's Workspace directory.
func (runtime *KubeRuntime) CopyFrom(ctx context.Context, target Target, path string, output io.Writer) error {
	return runtime.copy(ctx, target, "copy-from", path, nil, output)
}

// CopyTo unpacks a tar stream from input at path in the target runtime.
func (runtime *KubeRuntime) CopyTo(ctx context.Context, target Target, path string, input io.Reader) error {
	return runtime.copy(ctx, target, "copy-to", path, input, io.Discard)
}

func (runtime *KubeRuntime) copy(ctx context.Context, target Target, action, path string, input io.Reader, output io.Writer) error {
	executable := target.Executable
	if executable == "" {
		executable = runtimeBridgeCommand
	}
	stderr := new(bytes.Buffer)
	if err := runtime.executor.Exec(ctx, target, []string{executable, action, path}, input, output, stderr, false); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return fmt.Errorf("%s %s: %w: %s", action, path, err, message)
		}
		return fmt.Errorf("%s %s: %w", action, path, err)
	}

	return nil
}

// List returns every process the target supervisor has recorded, newest first.
func (runtime *KubeRuntime) List(ctx context.Context, target Target) ([]HistoryEntry, error) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	if err := runtime.executor.Exec(ctx, target, bridgeCommand(target, "list"), nil, stdout, stderr, false); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return nil, fmt.Errorf("exec rc-kube bridge: %w: %s", err, message)
		}
		return nil, fmt.Errorf("exec rc-kube bridge: %w", err)
	}
	entries := []HistoryEntry{}
	if err := json.NewDecoder(stdout).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode rc-kube process history: %w", err)
	}

	return entries, nil
}
