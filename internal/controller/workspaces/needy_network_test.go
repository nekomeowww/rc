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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	testClusterRange    = "10.0.0.0/8"
	testProxySecret     = "proxy-login"
	testProxyCredential = "login"
)

func TestNeedyPoliciesKeepActorsOutOfTheCluster(t *testing.T) {
	config := needyTestConfig()
	config.GatewayBlockCIDRs = []string{testClusterRange}
	config.NetworkPolicies = true
	config.ControllerNamespace = "rc-system"
	harness := newNeedyHarness(t, config, executorWorkspace())
	harness.reconcile()

	var actor networkingv1.NetworkPolicy
	require.NoError(t, harness.get(needyActorPolicyName, &actor))
	assert.Equal(t, map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentActor}, actor.Spec.PodSelector.MatchLabels)
	assert.ElementsMatch(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, actor.Spec.PolicyTypes)
	assert.Empty(t, actor.Spec.Ingress, "nothing dials an actor runner")
	internet := actor.Spec.Egress[0].To[0].IPBlock
	require.NotNil(t, internet)
	assert.Equal(t, "0.0.0.0/0", internet.CIDR)
	assert.Equal(t, []string{testClusterRange, needyMetadataCIDR}, internet.Except)
	daemon := actor.Spec.Egress[1]
	assert.Equal(t, needyDaemonSelector(), daemon.To[0].PodSelector.MatchLabels)
	assert.Equal(t, int32(needyControlPort), daemon.Ports[0].Port.IntVal)
	assert.Len(t, actor.Spec.Egress, 3, "the internet, the daemon's control port, and DNS; no gateway port")

	var daemonPolicy networkingv1.NetworkPolicy
	require.NoError(t, harness.get(needyDaemonPolicyName, &daemonPolicy))
	sources := daemonPolicy.Spec.Ingress[0].From
	require.Len(t, sources, 3)
	assert.Equal(t, needyComponentActor, sources[0].PodSelector.MatchLabels[workspacesv1alpha1.NeedyComponentLabel])
	assert.Equal(t, workspaceManagedByLabel, sources[1].PodSelector.MatchExpressions[0].Key)
	assert.Equal(t, "rc-system", sources[2].NamespaceSelector.MatchLabels[corev1.LabelMetadataName])
	assert.Equal(t, int32(needyControlPort), daemonPolicy.Spec.Ingress[0].Ports[0].Port.IntVal)
}

func TestActorProxyPolicy(t *testing.T) {
	selected, err := actorProxyPolicy(testActor(), &workspacesv1alpha1.ActorProxy{
		URL:         "socks5h://proxy:1080",
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "proxy"}},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{workspacesv1alpha1.NeedyActorLabel: testActorName}, selected.Spec.PodSelector.MatchLabels)
	assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, selected.Spec.PolicyTypes)
	assert.Equal(t, map[string]string{"app": "proxy"}, selected.Spec.Egress[0].To[0].PodSelector.MatchLabels)
	assert.Equal(t, int32(1080), selected.Spec.Egress[0].Ports[0].Port.IntVal)

	address, err := actorProxyPolicy(testActor(), &workspacesv1alpha1.ActorProxy{URL: "socks5h://203.0.113.7:1081"})
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.7/32", address.Spec.Egress[0].To[0].IPBlock.CIDR)

	external, err := actorProxyPolicy(testActor(), &workspacesv1alpha1.ActorProxy{URL: "socks5h://proxy.example.com:1080"})
	require.NoError(t, err)
	assert.Nil(t, external, "a proxy on the internet is reachable under the actor policy")
}

func TestActorRunsThroughItsProxy(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.reconciler.Config.NetworkPolicies = true
	actor := harness.actor()
	actor.Spec.Proxy = &workspacesv1alpha1.ActorProxy{
		URL: "socks5h://10.1.2.3:1080",
		CredentialsSecretRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: testProxySecret},
			Key:                  testProxyCredential,
		},
	}
	require.NoError(t, harness.client.Update(context.Background(), actor))
	harness.reconcile()

	pod := harness.pod()
	assert.Equal(t, testActorName, pod.Labels[workspacesv1alpha1.NeedyActorLabel])
	command := strings.Join(pod.Spec.Containers[0].Command, " ")
	assert.Contains(t, command, "--gateway-upstream socks5h://10.1.2.3:1080 --gateway-upstream-credentials-file /etc/needy/proxy/credentials")
	var volume *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == actorProxyVolume {
			volume = &pod.Spec.Volumes[i]
		}
	}
	require.NotNil(t, volume)
	assert.Equal(t, testProxySecret, volume.Secret.SecretName)
	assert.Equal(t, []corev1.KeyToPath{
		{
			Key:  testProxyCredential,
			Path: "credentials",
		},
	}, volume.Secret.Items)
	assert.Equal(t, int32(0o440), *volume.Secret.DefaultMode)

	var policy networkingv1.NetworkPolicy
	require.NoError(t, harness.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testActorName + "-needy-proxy"}, &policy))
	assert.Equal(t, "10.1.2.3/32", policy.Spec.Egress[0].To[0].IPBlock.CIDR)
	assert.Equal(t, testActorName, policy.OwnerReferences[0].Name)
}

func TestNeedyKeepsNoPoliciesUnlessAsked(t *testing.T) {
	harness := newNeedyHarness(t, needyTestConfig(), executorWorkspace())
	harness.reconcile()

	var policies networkingv1.NetworkPolicyList
	require.NoError(t, harness.client.List(context.Background(), &policies))
	assert.Empty(t, policies.Items)
}

func TestActorProxyWithoutPoliciesAddsNone(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	actor := harness.actor()
	actor.Spec.Proxy = &workspacesv1alpha1.ActorProxy{URL: "socks5h://10.1.2.3:1080"}
	require.NoError(t, harness.client.Update(context.Background(), actor))
	harness.reconcile()

	assert.Contains(t, strings.Join(harness.pod().Spec.Containers[0].Command, " "), "--gateway-upstream socks5h://10.1.2.3:1080")
	var policies networkingv1.NetworkPolicyList
	require.NoError(t, harness.client.List(context.Background(), &policies))
	assert.Empty(t, policies.Items, "an Egress policy for the proxy alone would cut the Pod off from the daemon")
}
