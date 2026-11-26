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

package workspaces

import (
	"fmt"
	"hash/fnv"
	"strings"

	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation"
)

// How rc names and places needy in a namespace. pkg/needy knows needy's
// formats; these are rc's own choices.
const (
	// needyDaemonName names the namespace's daemon Deployment and Service.
	needyDaemonName = "needy-daemon"
	// needyControlPort serves the daemon's gRPC API.
	needyControlPort = 7400
	// needyGatewayPort is the daemon's shared actor gateway.
	needyGatewayPort = 7401
	// needyCredentialKey is the credential in a runner Secret.
	needyCredentialKey = "credential"
	// needyFilesSecretName holds the daemon's static runners and clients
	// files, which hold only hashes.
	needyFilesSecretName = "needy-daemon-files"
	// needyRunnersFileKey and needyClientsFileKey are the files in it.
	needyRunnersFileKey = "runners.yaml"
	needyClientsFileKey = "clients.yaml"
	// needyClientSecretName holds the manager's admin client token; only the
	// manager reads it.
	needyClientSecretName = "needy-daemon-client"
	// needyClientTokenKey is the token in it.
	needyClientTokenKey = "token"
	// needyControllerClientName is the manager's static client on every
	// daemon; traces name it client:rc-controller.
	needyControllerClientName = "rc-controller"
	// needyStateClaimName is the daemon's state volume when a storage class
	// is configured.
	needyStateClaimName = "needy-daemon-state"
)

// Values of workspacesv1alpha1.NeedyComponentLabel.
const (
	needyComponentDaemon   = "daemon"
	needyComponentClient   = "client"
	needyComponentExecutor = "executor"
)

// needyDaemonAddress is the daemon's control address in a namespace.
func needyDaemonAddress(namespace string) string {
	return fmt.Sprintf("%s.%s.svc:%d", needyDaemonName, namespace, needyControlPort)
}

// needyExecutorSecretName holds a Workspace's executor runner credential.
func needyExecutorSecretName(workspace string) string {
	return workspace + "-needy-runner"
}

// needyWorkspaceRunnerName is the executor runner name of a Workspace.
func needyWorkspaceRunnerName(workspace string) string {
	return needyRunnerName("ws-", workspace)
}

// needyRunnerName joins prefix and name into a runner name, which needy
// requires to be a DNS label. A name that does not fit keeps its start and
// gets a hash of the whole name, as Kubernetes names generated objects.
func needyRunnerName(prefix, name string) string {
	if len(prefix)+len(name) <= validation.DNS1123LabelMaxLength {
		return prefix + name
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(name))
	suffix := "-" + rand.SafeEncodeString(fmt.Sprint(hasher.Sum32()))
	keep := validation.DNS1123LabelMaxLength - len(prefix) - len(suffix)

	return prefix + strings.TrimRight(name[:keep], "-") + suffix
}
