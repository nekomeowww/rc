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

// Package lifecycle executes ordered Workspace runtime lifecycle actions.
package lifecycle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
)

const DefaultWorkingDirectory = "/workspace"

// Action is the runtime representation of one exact argv or shell script.
type Action struct {
	Command          []string `json:"command,omitempty"`
	Script           string   `json:"script,omitempty"`
	WorkingDirectory string   `json:"workingDirectory,omitempty"`
}

// Encode serializes actions into one argument safe for a Pod command.
func Encode(actions []Action) (string, error) {
	data, err := json.Marshal(actions)
	if err != nil {
		return "", fmt.Errorf("marshal lifecycle actions: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(data), nil
}

// Decode restores actions encoded by Encode.
func Decode(encoded string) ([]Action, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode lifecycle actions: %w", err)
	}
	actions := make([]Action, 0)
	if err := json.Unmarshal(data, &actions); err != nil {
		return nil, fmt.Errorf("unmarshal lifecycle actions: %w", err)
	}

	return actions, nil
}

// Run executes actions serially and stops at the first failure.
func Run(ctx context.Context, actions []Action, stdin io.Reader, stdout, stderr io.Writer) error {
	for index, action := range actions {
		command, err := actionCommand(ctx, action)
		if err != nil {
			return fmt.Errorf("prepare lifecycle action %d: %w", index+1, err)
		}
		command.Stdin = stdin
		command.Stdout = stdout
		command.Stderr = stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("run lifecycle action %d: %w", index+1, err)
		}
	}

	return nil
}

func actionCommand(ctx context.Context, action Action) (*exec.Cmd, error) {
	hasCommand := len(action.Command) > 0
	hasScript := action.Script != ""
	if hasCommand == hasScript {
		return nil, fmt.Errorf("exactly one of command or script must be set")
	}
	var command *exec.Cmd
	if hasCommand {
		if action.Command[0] == "" {
			return nil, fmt.Errorf("command executable must not be empty")
		}
		command = exec.CommandContext(ctx, action.Command[0], action.Command[1:]...)
	} else {
		command = exec.CommandContext(ctx, "/bin/sh", "-ceu", action.Script, "rc-lifecycle")
	}
	command.Dir = action.WorkingDirectory
	if command.Dir == "" {
		command.Dir = DefaultWorkingDirectory
	}

	return command, nil
}
