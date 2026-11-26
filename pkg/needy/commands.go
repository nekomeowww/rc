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

// The words of needy's command line that every command shares.
const (
	cliNeedy   = "needy"
	cliRunner  = "runner"
	cliServe   = "serve"
	cliAddress = "--address"
	cliRoles   = "--roles"
)

// DaemonServeOptions configures `needy daemon serve`.
type DaemonServeOptions struct {
	// ControlListen and GatewayListen are tcp://HOST:PORT listeners.
	ControlListen string
	GatewayListen string
	// RunnersFile and ClientsFile are the static runners and clients files.
	RunnersFile string
	ClientsFile string
	// GatewayBlockCIDRs are ranges that the gateway refuses.
	GatewayBlockCIDRs []string
}

// DaemonServeCommand is the argv of a daemon for a shared cluster. It
// refuses host file mappings, whose host would be the daemon's own Pod, and
// reads its runners and clients from files provisioned before it starts.
func DaemonServeCommand(options DaemonServeOptions) []string {
	command := make([]string, 0, 13+2*len(options.GatewayBlockCIDRs))
	command = append(command,
		cliNeedy, "daemon", cliServe,
		"--listen", options.ControlListen,
		"--gateway-listen", options.GatewayListen,
		"--runners-file", options.RunnersFile,
		"--clients-file", options.ClientsFile,
		"--disable-host-files",
	)
	for _, cidr := range options.GatewayBlockCIDRs {
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
		cliNeedy, "config", "context", "check", "local",
		"-q",
		"--timeout", "2s",
	}
}

// ActorRunnerOptions configures an ephemeral actor runner.
type ActorRunnerOptions struct {
	// Address is the daemon's tcp://HOST:PORT control address.
	Address string
	// Gateway is the daemon's tcp://HOST:PORT gateway, used only when the
	// daemon is older than the runner and keeps the gateway itself.
	Gateway string
	// CredentialFile holds the runner's credential.
	CredentialFile string
	// WorkDirectory holds the VM's files.
	WorkDirectory string
	// Upstream sends the gateway's connections through a SOCKS5 proxy:
	// socks5h://HOST:PORT or socks5://HOST:PORT. Empty dials directly.
	Upstream string
	// UpstreamCredentialsFile holds USERNAME:PASSWORD for Upstream. needy
	// refuses a file that other users can read.
	UpstreamCredentialsFile string
}

// ActorRunnerCommand is the argv of an ephemeral actor runner: it serves one
// actor in a Firecracker VM and exits when that actor ends.
func ActorRunnerCommand(options ActorRunnerOptions) []string {
	command := []string{
		cliNeedy, cliRunner, cliServe,
		cliAddress, options.Address,
		"--credential-file", options.CredentialFile,
		cliRoles, string(RoleActor),
		"--ephemeral",
		"--gateway", options.Gateway,
		"--work-dir", options.WorkDirectory,
	}
	if options.Upstream != "" {
		command = append(command, "--gateway-upstream", options.Upstream)
		if options.UpstreamCredentialsFile != "" {
			command = append(command, "--gateway-upstream-credentials-file", options.UpstreamCredentialsFile)
		}
	}

	return command
}

// ExecutorRunnerOptions configures an executor runner.
type ExecutorRunnerOptions struct {
	// Executable is the needy binary.
	Executable string
	// Name is the runner's entry in the daemon's runners file. needy cannot
	// read it from a static credential.
	Name string
	// Address is the daemon's tcp://HOST:PORT control address.
	Address string
	// CredentialVariable names the environment variable with the credential.
	CredentialVariable string
	// ActivityHook runs with sh -c while exec streams carry traffic.
	ActivityHook string
}

// ExecutorRunnerCommand is the argv of an executor runner.
func ExecutorRunnerCommand(options ExecutorRunnerOptions) []string {
	return []string{
		options.Executable, cliRunner, cliServe,
		cliAddress, options.Address,
		"--name", options.Name,
		"--credential-env", options.CredentialVariable,
		cliRoles, string(RoleExecutor),
		"--activity-hook", options.ActivityHook,
	}
}
