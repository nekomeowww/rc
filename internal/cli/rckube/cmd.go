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
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
	runtime "github.com/nekomeowww/rc/internal/rckube"
)

// NewCommand creates the in-Pod runtime and bridge command tree.
func NewCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "rc-kube",
		Short:         "Supervise rc Workspace processes",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.AddCommand(newServeCommand(), newProcessCommand())

	return root
}

func newServeCommand() *cobra.Command {
	var socketPath string
	var stateDirectory string
	var stopGrace time.Duration
	var maxTranscriptBytes int64
	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the Workspace process supervisor",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			supervisor := runtime.NewSupervisor(stateDirectory, stopGrace, runtime.WithMaxTranscriptBytes(maxTranscriptBytes))
			return runtime.NewServer(supervisor).Serve(command.Context(), socketPath)
		},
	}
	command.Flags().StringVar(&socketPath, "socket", processruntime.DefaultSocketPath, "Unix socket path")
	command.Flags().StringVar(&stateDirectory, "state-dir", "/home/agent/.rc/processes", "Persistent process state directory")
	command.Flags().DurationVar(&stopGrace, "stop-grace", 10*time.Second, "SIGTERM grace period before SIGKILL")
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
	command.Flags().StringVar(&socketPath, "socket", processruntime.DefaultSocketPath, "Unix socket path")

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
	command.Flags().StringVar(&socketPath, "socket", processruntime.DefaultSocketPath, "Unix socket path")

	return command
}

func newAttachCommand() *cobra.Command {
	var socketPath string
	var clientID string
	command := &cobra.Command{
		Use:   "attach ID",
		Short: "Attach stdin and stdout to a supervised process",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return runtime.NewClient(socketPath).Attach(command.Context(), arguments[0], clientID, command.InOrStdin(), command.OutOrStdout())
		},
	}
	command.Flags().StringVar(&socketPath, "socket", processruntime.DefaultSocketPath, "Unix socket path")
	command.Flags().StringVar(&clientID, "client-id", "", "Stable identity for foreground terminal arbitration")

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
	command.Flags().StringVar(&socketPath, "socket", processruntime.DefaultSocketPath, "Unix socket path")

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
	command.Flags().StringVar(&socketPath, "socket", processruntime.DefaultSocketPath, "Unix socket path")
	command.Flags().Uint16Var(&rows, "rows", 0, "Terminal rows")
	command.Flags().Uint16Var(&columns, "columns", 0, "Terminal columns")
	command.Flags().StringVar(&clientID, "client-id", "", "Attach client controlling the shared viewport")
	_ = command.MarkFlagRequired("rows")
	_ = command.MarkFlagRequired("columns")

	return command
}
