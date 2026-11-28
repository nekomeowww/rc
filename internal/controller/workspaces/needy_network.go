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
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "github.com/nekomeowww/rc/apis/kubernetes/workspaces/v1alpha1"
)

// NetworkPolicy names in a namespace that runs needy.
const (
	needyActorPolicyName  = "needy-actor"
	needyDaemonPolicyName = "needy-daemon"
	// managerPodLabel and managerPodValue select the manager's Pods.
	managerPodLabel = "control-plane"
	managerPodValue = "controller-manager"
)

// needyInternet is the internet without the cluster's ranges and the cloud
// metadata address. Under a CNI that replaces kube-proxy a ClusterIP may be
// rewritten to a node address before policy applies, so the node ranges
// belong in config.GatewayBlockCIDRs too.
func needyInternet(config NeedyConfig) networkingv1.NetworkPolicyPeer {
	except := append([]string{}, config.GatewayBlockCIDRs...)
	except = append(except, needyMetadataCIDR)

	return networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: except}}
}

func needyDNSRule() networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: "kube-system"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			},
		},
		Ports: []networkingv1.NetworkPolicyPort{
			tcpOrUDP(corev1.ProtocolUDP, 53),
			tcpOrUDP(corev1.ProtocolTCP, 53),
		},
	}
}

func tcpOrUDP(protocol corev1.Protocol, port int32) networkingv1.NetworkPolicyPort {
	value := intstr.FromInt32(port)

	return networkingv1.NetworkPolicyPort{
		Protocol: &protocol,
		Port:     &value,
	}
}

// needyActorPolicy confines every actor runner Pod in a namespace. The
// runner's gateway dials the actor's allowed hosts itself, so the Pod reaches
// the internet, but not the cluster or the metadata address; the runner
// refuses the same ranges for the addresses the actor names. Nothing dials
// an actor runner Pod.
func needyActorPolicy(config NeedyConfig, namespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      needyActorPolicyName,
			Labels:    map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentActor},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentActor}},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{needyInternet(config)},
				},
				{
					To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: needyDaemonSelector()}}},
					Ports: []networkingv1.NetworkPolicyPort{tcpOrUDP(corev1.ProtocolTCP, needyControlPort)},
				},
				needyDNSRule(),
			},
		},
	}
}

// needyDaemonPolicy admits the namespace's actor runners and executor
// Workspaces, and the manager, to the daemon's control port. The daemon
// itself dials only the internet: the base VM image and image layers.
//
// TODO(external-executor-ingress): the runner of an Actor's external
// executor, outside the cluster, is not admitted, so with these policies it
// cannot connect and the Actor fails with ExecutorRunnerOffline. There is no
// ingress for it yet: external runners are meant to arrive through a cluster
// byte-stream router that carries their gRPC to the namespace's daemon (the
// needy Phase 3 data-plane design), which is not built. Admit that router's
// Pods here when it lands, rather than opening the control port.
func needyDaemonPolicy(config NeedyConfig, namespace string) *networkingv1.NetworkPolicy {
	sources := []networkingv1.NetworkPolicyPeer{
		{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentActor}}},
		{PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      workspaceManagedByLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		}}},
	}
	if config.ControllerNamespace != "" {
		sources = append(sources, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: config.ControllerNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{managerPodLabel: managerPodValue}},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      needyDaemonPolicyName,
			Labels:    map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentDaemon},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: needyDaemonSelector()},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From:  sources,
					Ports: []networkingv1.NetworkPolicyPort{tcpOrUDP(corev1.ProtocolTCP, needyControlPort)},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{needyInternet(config)},
				},
				needyDNSRule(),
			},
		},
	}
}

// actorProxyPolicyName names the policy that admits one Actor to its proxy.
func actorProxyPolicyName(actor *workspacesv1alpha1.Actor) string {
	return actor.Name + "-needy-proxy"
}

// actorProxyPolicy admits one actor runner Pod to its proxy: the selected
// Pods, or the URL's address when it is an IP address. A proxy named by a
// host outside the cluster is reachable through needyActorPolicy already.
func actorProxyPolicy(actor *workspacesv1alpha1.Actor, proxy *workspacesv1alpha1.ActorProxy) (*networkingv1.NetworkPolicy, error) {
	parsed, err := url.Parse(proxy.URL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	port, err := strconv.ParseInt(parsed.Port(), 10, 32)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("proxy URL %s has no valid port", proxy.URL)
	}
	var peer networkingv1.NetworkPolicyPeer
	switch address := net.ParseIP(parsed.Hostname()); {
	case proxy.PodSelector != nil:
		peer = networkingv1.NetworkPolicyPeer{PodSelector: proxy.PodSelector.DeepCopy()}
	case address != nil:
		bits := 32
		if address.To4() == nil {
			bits = 128
		}
		peer = networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: fmt.Sprintf("%s/%d", address, bits)}}
	default:
		return nil, nil
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: actor.Namespace,
			Name:      actorProxyPolicyName(actor),
			Labels:    map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentActor},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{workspacesv1alpha1.NeedyActorLabel: actor.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To:    []networkingv1.NetworkPolicyPeer{peer},
					Ports: []networkingv1.NetworkPolicyPort{tcpOrUDP(corev1.ProtocolTCP, int32(port))},
				},
			},
		},
	}, nil
}

func (r *NeedyReconciler) ensureNetworkPolicies(ctx context.Context, namespace string) error {
	for _, desired := range []*networkingv1.NetworkPolicy{
		needyActorPolicy(r.Config, namespace),
		needyDaemonPolicy(r.Config, namespace),
	} {
		policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: desired.Name}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
			policy.Labels = desired.Labels
			policy.Spec = desired.Spec

			return nil
		}); err != nil {
			return fmt.Errorf("ensure NetworkPolicy %s: %w", desired.Name, err)
		}
	}

	return nil
}
