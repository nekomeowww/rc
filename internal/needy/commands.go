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

package needy

import "fmt"

// Values of ComponentLabel.
const (
	ComponentDaemon   = "daemon"
	ComponentClient   = "client"
	ComponentExecutor = "executor"
	ComponentActor    = "actor"
)

// needy's runner roles, as the CLI and the runners file spell them.
const (
	RoleActor    = "actor"
	RoleExecutor = "executor"
)

// DaemonServeCommand is the argv of the per-namespace daemon. It refuses host
// file mappings, whose host would be the daemon's own Pod, keeps the gateway
// away from blockCIDRs, and reads its runners and clients from files that the
// manager provisions.
func DaemonServeCommand(runnersFile, clientsFile string, blockCIDRs []string) []string {
	command := make([]string, 0, 13+2*len(blockCIDRs))
	command = append(command,
		"needy", "daemon", "serve",
		"--listen", fmt.Sprintf("tcp://0.0.0.0:%d", ControlPort),
		"--gateway-listen", fmt.Sprintf("tcp://0.0.0.0:%d", GatewayPort),
		"--runners-file", runnersFile,
		"--clients-file", clientsFile,
		"--disable-host-files",
	)
	for _, cidr := range blockCIDRs {
		command = append(command, "--gateway-block-cidr", cidr)
	}

	return command
}

// DaemonCheckCommand checks the daemon's health through its management
// socket, inside its own container. A TCP probe would come from the node,
// which a NetworkPolicy may refuse under a CNI that applies policy to host
// traffic.
func DaemonCheckCommand() []string {
	return []string{
		"needy", "config", "context", "check", "local",
		"-q",
		"--timeout", "2s",
	}
}

// ActorRunnerCommand is the argv of an ephemeral actor runner: it serves one
// actor in a Firecracker VM and exits when that actor ends. Its gateway runs
// in the runner; a daemon older than the runner gets the daemon's gateway at
// --gateway instead. An upstream sends the gateway's connections through a
// SOCKS5 proxy; credentialsFile may be empty.
func ActorRunnerCommand(namespace, credentialFile, workDirectory, upstream, credentialsFile string) []string {
	command := []string{
		"needy", "runner", "serve",
		"--address", "tcp://" + DaemonAddress(namespace),
		"--credential-file", credentialFile,
		"--roles", RoleActor,
		"--ephemeral",
		"--gateway", "tcp://" + GatewayAddress(namespace),
		"--work-dir", workDirectory,
	}
	if upstream != "" {
		command = append(command, "--gateway-upstream", upstream)
		if credentialsFile != "" {
			command = append(command, "--gateway-upstream-credentials-file", credentialsFile)
		}
	}

	return command
}

// ExecutorRunnerCommand is the argv of a Workspace's executor runner. The
// activity hook runs while exec streams carry traffic.
func ExecutorRunnerCommand(executable, namespace, credentialVariable, activityHook string) []string {
	return []string{
		executable, "runner", "serve",
		"--address", "tcp://" + DaemonAddress(namespace),
		"--credential-env", credentialVariable,
		"--roles", RoleExecutor,
		"--activity-hook", activityHook,
	}
}
