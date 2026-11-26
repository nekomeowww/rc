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

// Package needy holds what rc knows about needy: runner credentials, the
// static runners file, profile composition, the daemon's gRPC client, and the
// manager configuration. needy owns routing, the gateway, traces, and the
// profile schema; rc only places the daemon, the actor runner, and the
// executor runners on Kubernetes.
package needy

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Names and ports shared by the daemon, the runners, and the manager.
const (
	// DaemonName names the per-namespace daemon Deployment and Service.
	DaemonName = "needy-daemon"
	// FilesSecretName holds the daemon's static runners and clients files.
	// It holds only hashes.
	FilesSecretName = "needy-daemon-files"
	// ClientSecretName holds the manager's admin client token. Only the
	// manager reads it.
	ClientSecretName = "needy-daemon-client"
	// ControllerClientName is the manager's static client on every daemon;
	// traces name it client:rc-controller.
	ControllerClientName = "rc-controller"
	// StateClaimName is the daemon's state volume when a storage class is set.
	StateClaimName = "needy-daemon-state"
	// RunnersFileKey is the runners file in FilesSecretName.
	RunnersFileKey = "runners.yaml"
	// ClientsFileKey is the clients file in FilesSecretName.
	ClientsFileKey = "clients.yaml"
	// ClientTokenKey is the token in ClientSecretName.
	ClientTokenKey = "token"
	// CredentialKey is the runner credential in a runner Secret.
	CredentialKey = "credential"
	// ControlPort serves the daemon's gRPC API to the manager and the runners.
	ControlPort = 7400
	// GatewayPort is the daemon's shared actor gateway.
	GatewayPort = 7401
	// BinaryDirectory holds the needy that the manager installs into a
	// Workspace runtime.
	BinaryDirectory = "/opt/needy/bin"
	// ImageExecutable is needy in the needy image.
	ImageExecutable = "/usr/local/bin/needy"
	// WorkspaceLabel names the Workspace that an executor runner serves.
	WorkspaceLabel = "rc.ayaka.io/workspace"
	// ComponentLabel marks the objects that the manager creates for needy.
	ComponentLabel = "rc.ayaka.io/needy"
	// ActorLabel names the Actor of an actor runner Pod.
	ActorLabel = "rc.ayaka.io/actor"
	// MetadataCIDR is the cloud metadata address, which no needy Pod may reach.
	MetadataCIDR = "169.254.169.254/32"
)

// Config is the manager's needy configuration. needy is disabled while Image
// is empty.
type Config struct {
	// Image provides needy at ImageExecutable, a shell, and CA roots. The
	// daemon and the actor runner run in it; Workspaces copy needy from it.
	Image string
	// KVMResource is the extended resource that exposes /dev/kvm.
	KVMResource corev1.ResourceName
	// KVMNodeSelector and KVMTolerations place Pods that need KVM.
	KVMNodeSelector map[string]string
	KVMTolerations  []corev1.Toleration
	// KVMGroup is the group that owns /dev/kvm on the KVM nodes. Zero adds no
	// supplemental group.
	KVMGroup int64
	// DaemonKVM places the daemon on a KVM node with the KVM device, so it can
	// build image steps itself. Without it, profiles that need a build fail
	// until a layer registry or a builder holds every step.
	DaemonKVM bool
	// DaemonStorageClass keeps the daemon's state on a PersistentVolumeClaim.
	// Empty keeps it in an emptyDir, which loses paired clients and run
	// history on every daemon restart.
	DaemonStorageClass string
	// GatewayBlockCIDRs are passed as --gateway-block-cidr: the cluster's Pod,
	// Service, and node ranges. The NetworkPolicies exclude them from the
	// internet too.
	GatewayBlockCIDRs []string
	// UpstreamImage is the Mihomo image of the managed upstream. Empty
	// disables managed upstreams.
	UpstreamImage string
	// ControllerNamespace is the manager's namespace, whose manager Pods may
	// reach each daemon. Empty admits no manager Pod, which suits a manager
	// outside the cluster that reaches the daemon through a port-forward.
	ControllerNamespace string
}

// Enabled reports whether the manager runs needy.
func (config Config) Enabled() bool {
	return config.Image != ""
}

// ActorReady reports why Actors cannot run, or "" when they can.
func (config Config) ActorReady() string {
	switch {
	case !config.Enabled():
		return "the manager has no needy image (--needy-image)"
	case config.KVMResource == "":
		return "the manager has no KVM resource (--needy-kvm-resource)"
	}

	return ""
}

// DaemonAddress is the daemon's control address in a namespace.
func DaemonAddress(namespace string) string {
	return fmt.Sprintf("%s.%s.svc:%d", DaemonName, namespace, ControlPort)
}

// GatewayAddress is the daemon's gateway address in a namespace.
func GatewayAddress(namespace string) string {
	return fmt.Sprintf("%s.%s.svc:%d", DaemonName, namespace, GatewayPort)
}

// ParseSelector parses key=value pairs separated by commas.
func ParseSelector(value string) (map[string]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	selector := make(map[string]string)
	for pair := range strings.SplitSeq(value, ",") {
		key, label, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found || key == "" {
			return nil, fmt.Errorf("node selector entry %q must be key=value", pair)
		}
		selector[key] = label
	}

	return selector, nil
}

// ParseTolerations parses key[=value]:Effect entries separated by commas.
func ParseTolerations(value string) ([]corev1.Toleration, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var tolerations []corev1.Toleration
	for entry := range strings.SplitSeq(value, ",") {
		keyValue, effect, found := strings.Cut(strings.TrimSpace(entry), ":")
		if !found || keyValue == "" || effect == "" {
			return nil, fmt.Errorf("toleration %q must be key[=value]:Effect", entry)
		}
		toleration := corev1.Toleration{
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffect(effect),
		}
		toleration.Key, toleration.Value, found = strings.Cut(keyValue, "=")
		if found {
			toleration.Operator = corev1.TolerationOpEqual
		}
		tolerations = append(tolerations, toleration)
	}

	return tolerations, nil
}
