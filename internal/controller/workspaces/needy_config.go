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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// NeedyConfig is the manager's needy configuration. needy is off while Image
// is empty.
type NeedyConfig struct {
	// Image provides needy at needy.ImageExecutable, a shell, and CA roots.
	// The daemon and the actor runner run in it; Workspaces copy needy from
	// it.
	Image string
	// KVMResource is the extended resource that exposes /dev/kvm.
	KVMResource corev1.ResourceName
	// KVMNodeSelector and KVMTolerations place Pods that need KVM.
	KVMNodeSelector labels.Set
	KVMTolerations  []corev1.Toleration
	// KVMGroup is the group that owns /dev/kvm on the KVM nodes. Zero adds no
	// supplemental group.
	KVMGroup int64
	// DaemonKVM places the daemon on a KVM node with the KVM device, so it can
	// build image steps itself.
	DaemonKVM bool
	// DaemonStorageClass keeps the daemon's state on a PersistentVolumeClaim.
	// Empty keeps it in an emptyDir, which loses run history and the daemon's
	// records on every restart.
	DaemonStorageClass string
	// GatewayBlockCIDRs are ranges that the actor gateway refuses: the
	// cluster's Pod, Service, and node ranges.
	GatewayBlockCIDRs []string
}

// IsEnabled reports whether the manager runs needy.
func (config NeedyConfig) IsEnabled() bool {
	return config.Image != ""
}

// ActorUnavailableReason says why Actors cannot run, or is empty when they
// can.
func (config NeedyConfig) ActorUnavailableReason() string {
	switch {
	case !config.IsEnabled():
		return "the manager has no needy image (--needy-image)"
	case config.KVMResource == "":
		return "the manager has no KVM resource (--needy-kvm-resource)"
	}

	return ""
}
