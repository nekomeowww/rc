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
	"errors"
	"fmt"
	"io"
	"strings"
)

const DefaultSocketPath = "/run/rc/rc-kube.sock"

const (
	runtimeBridgeCommand = "rc-kube"
	runtimeProcessGroup  = "process"
	runtimeSocketFlag    = "--socket"
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
		return State{}, fmt.Errorf("encode Agent Process start request: %w", err)
	}

	return runtime.stateCommand(ctx, target, []string{runtimeBridgeCommand, runtimeProcessGroup, "start", runtimeSocketFlag, DefaultSocketPath}, bytes.NewReader(data))
}

func (runtime *KubeRuntime) Inspect(ctx context.Context, target Target, id string) (State, error) {
	return runtime.stateCommand(ctx, target, []string{runtimeBridgeCommand, runtimeProcessGroup, "inspect", id, runtimeSocketFlag, DefaultSocketPath}, nil)
}

func (runtime *KubeRuntime) Stop(ctx context.Context, target Target, id string) (State, error) {
	return runtime.stateCommand(ctx, target, []string{runtimeBridgeCommand, runtimeProcessGroup, "stop", id, runtimeSocketFlag, DefaultSocketPath}, nil)
}

func (runtime *KubeRuntime) Attach(ctx context.Context, target Target, id string, clientID string, input io.Reader, output io.Writer, _ bool) error {
	// The child owns the PTY. The Kubernetes exec bridge stays a transparent
	// byte stream so it does not introduce a second terminal line discipline.
	return runtime.executor.Exec(ctx, target, []string{runtimeBridgeCommand, runtimeProcessGroup, "attach", id, runtimeSocketFlag, DefaultSocketPath, "--client-id", clientID}, input, output, output, false)
}

func (runtime *KubeRuntime) Logs(ctx context.Context, target Target, id string, output io.Writer) error {
	return runtime.executor.Exec(ctx, target, []string{runtimeBridgeCommand, runtimeProcessGroup, "logs", id, runtimeSocketFlag, DefaultSocketPath}, nil, output, output, false)
}

func (runtime *KubeRuntime) Resize(ctx context.Context, target Target, id string, clientID string, rows uint16, columns uint16) error {
	command := []string{runtimeBridgeCommand, runtimeProcessGroup, "resize", id, runtimeSocketFlag, DefaultSocketPath, "--client-id", clientID, "--rows", fmt.Sprint(rows), "--columns", fmt.Sprint(columns)}
	return runtime.executor.Exec(ctx, target, command, nil, io.Discard, io.Discard, false)
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
