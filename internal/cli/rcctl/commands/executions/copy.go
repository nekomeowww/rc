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

package executions

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/spf13/cobra"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/filecopy"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	workspaceservice "github.com/nekomeowww/rc/internal/workspaces"
)

// A remote operand is WORKSPACE:PATH. The Workspace part is a DNS label of at
// least two characters, so a Windows drive such as C:\dir stays local.
var remoteCopyPattern = regexp.MustCompile(`^([a-z0-9][-a-z0-9]*[a-z0-9]):(.*)$`)

type copyOperand struct {
	workspace string
	path      string
}

func parseCopyOperand(value string) copyOperand {
	if match := remoteCopyPattern.FindStringSubmatch(value); match != nil {
		return copyOperand{workspace: match[1], path: match[2]}
	}

	return copyOperand{path: value}
}

func newCopyCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "cp SOURCE DESTINATION",
		Short: "Copy files between this machine and a running Workspace",
		Long: "Copy a file or directory between this machine and a running Workspace. " +
			"Write the Workspace side as WORKSPACE:PATH; a relative PATH starts at the Workspace directory. " +
			"A destination that is an existing directory receives the source inside it; otherwise the copy takes the destination name. " +
			"The copy streams through the Workspace's rc-kube, not through a process, so size and binary content are not limited.",
		Example: "  rcctl cp ./screenshots dev:artifacts/\n  rcctl cp dev:/home/agent/report.html ./report.html",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			source, destination := parseCopyOperand(args[0]), parseCopyOperand(args[1])
			if (source.workspace == "") == (destination.workspace == "") {
				return errors.New("exactly one of SOURCE and DESTINATION must be WORKSPACE:PATH")
			}
			remote := source
			if remote.workspace == "" {
				remote = destination
			}
			if remote.path == "" {
				return fmt.Errorf("%s: name a path after the colon", remote.workspace)
			}
			processes, namespace, err := processClient(kubeconfigFlags)
			if err != nil {
				return err
			}
			target, err := processInventory(processes).RuntimeTarget(cmd.Context(), workspaceservice.InventoryTarget{
				Namespace: namespace,
				Kind:      workspacesv1alpha1.WorkspaceExecTargetWorkspace,
				Name:      remote.workspace,
			})
			if err != nil {
				return err
			}
			reader, writer := io.Pipe()
			if source.workspace != "" {
				go func() {
					_ = writer.CloseWithError(processes.Runtime.CopyFrom(cmd.Context(), target, source.path, writer))
				}()
				err := filecopy.Unpack(reader, destination.path)
				_ = reader.CloseWithError(err)
				return err
			}
			if _, err := os.Lstat(source.path); err != nil {
				return err
			}
			packed := make(chan error, 1)
			go func() {
				err := filecopy.Pack(writer, source.path)
				packed <- err
				_ = writer.CloseWithError(err)
			}()
			err = processes.Runtime.CopyTo(cmd.Context(), target, destination.path, reader)
			_ = reader.CloseWithError(err)
			// A local read failure truncates the stream; report it rather than
			// the remote unpack error it caused. A write failure only echoes the
			// remote error that closed the pipe.
			if packErr := <-packed; packErr != nil && !errors.Is(packErr, io.ErrClosedPipe) && (err == nil || !errors.Is(packErr, err)) {
				return fmt.Errorf("read %s: %w", source.path, packErr)
			}
			return err
		},
	}
}
