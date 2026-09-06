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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
	"github.com/nekomeowww/rc/internal/lifecycle"
	runtime "github.com/nekomeowww/rc/internal/rckube"
	"github.com/nekomeowww/rc/internal/rcnative"
)

// NewCommand creates the in-Pod runtime and bridge command tree.
func NewCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "rc-kube",
		Short:         "Supervise rc Workspace processes",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.AddCommand(newServeCommand(), newProcessCommand(), newLifecycleCommand(), newHealthCommand(), newTranscriptCommand())

	return root
}

func newLifecycleCommand() *cobra.Command {
	var encodedActions string
	command := &cobra.Command{
		Use:   "lifecycle",
		Short: "Run ordered Workspace lifecycle actions",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			actions, err := lifecycle.Decode(encodedActions)
			if err != nil {
				return err
			}

			return lifecycle.Run(command.Context(), actions, command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr())
		},
	}
	command.Flags().StringVar(&encodedActions, "actions", "", "Base64url-encoded lifecycle action list")
	_ = command.MarkFlagRequired("actions")

	return command
}

func newServeCommand() *cobra.Command {
	var socketPath string
	var stateDirectory string
	var stopGrace time.Duration
	var maxTranscriptBytes int64
	var initializeActions string
	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the Workspace process supervisor",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := os.MkdirAll(rcnative.Current().Workspace, 0o755); err != nil {
				return err
			}
			if initializeActions != "" {
				actions, err := lifecycle.Decode(initializeActions)
				if err != nil {
					return err
				}
				if err := lifecycle.Run(command.Context(), actions, nil, command.OutOrStdout(), command.ErrOrStderr()); err != nil {
					return err
				}
			}
			native := rcnative.Current()
			supervisor := runtime.NewSupervisor(stateDirectory, stopGrace,
				runtime.WithRoots(native.Home, native.Workspace, native.Run),
				runtime.WithMaxTranscriptBytes(maxTranscriptBytes),
			)
			return runtime.NewServer(supervisor).Serve(command.Context(), socketPath)
		},
	}
	command.Flags().StringVar(&socketPath, "socket", rcnative.Current().Endpoint, "Local Unix socket or Windows named pipe")
	command.Flags().StringVar(&stateDirectory, "state-dir", rcnative.Current().StateDirectory(), "Persistent process state directory")
	command.Flags().StringVar(&initializeActions, "initialize-actions", "", "Lifecycle actions run in the runtime container before readiness")
	command.Flags().DurationVar(&stopGrace, "stop-grace", 10*time.Second, "Process stop grace period before forced tree termination")
	command.Flags().Int64Var(&maxTranscriptBytes, "max-transcript-bytes", 64<<20, "Maximum durable bytes retained per process; zero disables")

	return command
}

func newProcessCommand() *cobra.Command {
	command := &cobra.Command{Use: "process", Short: "Bridge one request to the local supervisor"}
	command.AddCommand(newStartCommand(), newStateCommand("inspect"), newStateCommand("stop"), newAttachCommand(), newLogsCommand(), newResizeCommand())

	return command
}

func newStartCommand() *cobra.Command {
	var socketPath string
	command := &cobra.Command{
		Use:   "start",
		Short: "Start an idempotent process from a JSON request on stdin",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			request := processruntime.StartRequest{}
			if err := json.NewDecoder(command.InOrStdin()).Decode(&request); err != nil {
				return fmt.Errorf("decode start request: %w", err)
			}
			state, err := runtime.NewClient(socketPath).Start(command.Context(), request)
			if err != nil {
				return err
			}

			return json.NewEncoder(command.OutOrStdout()).Encode(state)
		},
	}
	command.Flags().StringVar(&socketPath, "socket", rcnative.Current().Endpoint, "Local Unix socket or Windows named pipe")

	return command
}

func newStateCommand(action string) *cobra.Command {
	var socketPath string
	command := &cobra.Command{
		Use:   action + " ID",
		Short: action + " one supervised process",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			client := runtime.NewClient(socketPath)
			var state processruntime.State
			var err error
			if action == "inspect" {
				state, err = client.Inspect(command.Context(), arguments[0])
			} else {
				state, err = client.Stop(command.Context(), arguments[0])
			}
			if err != nil {
				return err
			}

			return json.NewEncoder(command.OutOrStdout()).Encode(state)
		},
	}
	command.Flags().StringVar(&socketPath, "socket", rcnative.Current().Endpoint, "Local Unix socket or Windows named pipe")

	return command
}

func newAttachCommand() *cobra.Command {
	var socketPath string
	var clientID string
	var rows uint16
	var columns uint16
	command := &cobra.Command{
		Use:   "attach ID",
		Short: "Attach stdin and stdout to a supervised process",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return runtime.NewClient(socketPath).Attach(command.Context(), arguments[0], clientID, command.InOrStdin(), command.OutOrStdout(), rows, columns)
		},
	}
	command.Flags().StringVar(&socketPath, "socket", rcnative.Current().Endpoint, "Local Unix socket or Windows named pipe")
	command.Flags().StringVar(&clientID, "client-id", "", "Stable identity for foreground terminal arbitration")
	command.Flags().Uint16Var(&rows, "rows", 0, "Initial terminal rows")
	command.Flags().Uint16Var(&columns, "columns", 0, "Initial terminal columns")

	return command
}

func newLogsCommand() *cobra.Command {
	var socketPath string
	command := &cobra.Command{
		Use:   "logs ID",
		Short: "Read a supervised process transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return runtime.NewClient(socketPath).Logs(command.Context(), arguments[0], command.OutOrStdout())
		},
	}
	command.Flags().StringVar(&socketPath, "socket", rcnative.Current().Endpoint, "Local Unix socket or Windows named pipe")

	return command
}

func newResizeCommand() *cobra.Command {
	var socketPath string
	var rows uint16
	var columns uint16
	var clientID string
	command := &cobra.Command{
		Use:   "resize ID",
		Short: "Resize a supervised PTY",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return runtime.NewClient(socketPath).Resize(command.Context(), arguments[0], clientID, rows, columns)
		},
	}
	command.Flags().StringVar(&socketPath, "socket", rcnative.Current().Endpoint, "Local Unix socket or Windows named pipe")
	command.Flags().Uint16Var(&rows, "rows", 0, "Terminal rows")
	command.Flags().Uint16Var(&columns, "columns", 0, "Terminal columns")
	command.Flags().StringVar(&clientID, "client-id", "", "Attach client controlling the shared viewport")
	_ = command.MarkFlagRequired("rows")
	_ = command.MarkFlagRequired("columns")

	return command
}

func newHealthCommand() *cobra.Command {
	var address string
	command := &cobra.Command{Use: "health", Short: "Check local supervisor readiness", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(command.Context(), 2*time.Second)
			defer cancel()
			return runtime.NewClient(address).Ping(ctx)
		}}
	command.Flags().StringVar(&address, "socket", rcnative.Current().Endpoint, "Local supervisor endpoint")
	return command
}

// Read a transcript without exposing a general home-directory file reader.
func newTranscriptCommand() *cobra.Command {
	return &cobra.Command{Use: "transcript PROCESS-ID", Short: "Read a retained process transcript", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			processID := arguments[0]
			if filepath.Base(processID) != processID || processID == "." || processID == ".." {
				return fmt.Errorf("process ID must be one safe path segment")
			}
			root, err := os.OpenRoot(rcnative.Current().StateDirectory())
			if err != nil {
				return err
			}
			defer func() { _ = root.Close() }()
			file, err := root.Open(filepath.Join(processID, "transcript.log"))
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
			_, err = io.Copy(command.OutOrStdout(), file)
			return err
		}}
}
