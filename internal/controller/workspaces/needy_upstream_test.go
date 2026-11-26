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
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const testUpstreamImage = "example.com/proxy:1"

func managedUpstreamActor() *workspacesv1alpha1.Actor {
	actor := testActor()
	actor.Spec.Upstream = &workspacesv1alpha1.ActorUpstream{Managed: true}

	return actor
}

func TestMihomoUpstreamRunsOnlyWhileUsed(t *testing.T) {
	config := needyTestConfig()
	config.GatewayBlockCIDRs = []string{testClusterRange}
	harness := newNeedyHarness(t, config, executorWorkspace())
	harness.reconciler.Upstream = &MihomoUpstream{
		Client: harness.client,
		Scheme: harness.reconciler.Scheme,
		Config: config,
		Image:  testUpstreamImage,
	}
	harness.reconcile()
	assert.Error(t, harness.get(needyUpstreamName, &appsv1.Deployment{}), "nothing is created before an Actor uses it")

	require.NoError(t, harness.client.Create(context.Background(), managedUpstreamActor()))
	harness.reconcile()
	var deployment appsv1.Deployment
	require.NoError(t, harness.get(needyUpstreamName, &deployment))
	assert.Equal(t, int32(1), *deployment.Spec.Replicas)
	spec := deployment.Spec.Template.Spec
	container := spec.Containers[0]
	assert.Equal(t, testUpstreamImage, container.Image)
	assert.True(t, *container.SecurityContext.ReadOnlyRootFilesystem)
	assert.False(t, *spec.AutomountServiceAccountToken)
	require.NotNil(t, container.ReadinessProbe.Exec, "a TCP probe from the node may be refused by the upstream's policy")

	var credentials corev1.Secret
	require.NoError(t, harness.get(needyUpstreamCredentialsSecretName, &credentials))
	line := string(credentials.Data[actorUpstreamCredentialsFile])
	assert.True(t, strings.HasPrefix(line, needyUpstreamActorUser+":"))
	var configuration corev1.Secret
	require.NoError(t, harness.get(needyUpstreamConfigSecretName, &configuration))
	assert.Contains(t, string(configuration.Data[needyUpstreamConfigKey]), strings.TrimPrefix(line, needyUpstreamActorUser+":"))

	// The upstream's own policy is part of its filtering, with or without
	// the manager's other needy policies.
	var policy networkingv1.NetworkPolicy
	require.NoError(t, harness.get(needyUpstreamName, &policy))
	assert.Equal(t, needyComponentActor, policy.Spec.Ingress[0].From[0].PodSelector.MatchLabels[workspacesv1alpha1.NeedyComponentLabel])
	except := policy.Spec.Egress[0].To[0].IPBlock.Except
	assert.Contains(t, except, testClusterRange)
	assert.Contains(t, except, "169.254.0.0/16")

	harness.reconcile()
	require.NoError(t, harness.get(needyUpstreamCredentialsSecretName, &credentials))
	assert.Equal(t, line, string(credentials.Data[actorUpstreamCredentialsFile]), "the password is stable")

	var actor workspacesv1alpha1.Actor
	require.NoError(t, harness.get(testActorName, &actor))
	actor.Status.Phase = workspacesv1alpha1.ActorPhaseSucceeded
	require.NoError(t, harness.client.Update(context.Background(), &actor))
	harness.reconcile()
	require.NoError(t, harness.get(needyUpstreamName, &deployment))
	assert.Equal(t, int32(0), *deployment.Spec.Replicas)
}

func TestActorWaitsWithoutAnUpstreamProvider(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	actor := harness.actor()
	actor.Spec.Upstream = &workspacesv1alpha1.ActorUpstream{Managed: true}
	require.NoError(t, harness.client.Update(context.Background(), actor))
	harness.reconcile()

	actor = harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorReasonWaitingForUpstream, actor.Status.Conditions[0].Reason)
	assert.Contains(t, actor.Status.Conditions[0].Message, "--needy-upstream-provider")
	assert.Empty(t, harness.daemon.created)
}

func TestActorUsesTheManagedUpstreamByClusterIP(t *testing.T) {
	upstreamDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: needyUpstreamName, Namespace: testNamespace},
	}
	upstreamService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: needyUpstreamName, Namespace: testNamespace},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.96.7.7"},
	}
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon(), upstreamDeployment, upstreamService)
	harness.reconciler.Config.NetworkPolicies = true
	harness.reconciler.Upstream = &MihomoUpstream{
		Client: harness.client,
		Scheme: harness.reconciler.Scheme,
		Config: harness.reconciler.Config,
		Image:  testUpstreamImage,
	}
	actor := harness.actor()
	actor.Spec.Upstream = &workspacesv1alpha1.ActorUpstream{Managed: true}
	require.NoError(t, harness.client.Update(context.Background(), actor))

	harness.reconcile()
	actor = harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorReasonWaitingForUpstream, actor.Status.Conditions[0].Reason)
	assert.Empty(t, harness.daemon.created)

	upstreamDeployment.Status.AvailableReplicas = 1
	require.NoError(t, harness.client.Status().Update(context.Background(), upstreamDeployment))
	harness.reconcile()
	command := strings.Join(harness.pod().Spec.Containers[0].Command, " ")
	assert.Contains(t, command, "--gateway-upstream socks5h://10.96.7.7:1080")
	assert.Contains(t, command, "--gateway-upstream-credentials-file /etc/needy/upstream/credentials")
	var policy networkingv1.NetworkPolicy
	require.NoError(t, harness.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testActorName + "-needy-upstream"}, &policy))
	assert.Equal(t, needyUpstreamSelector(), policy.Spec.Egress[0].To[0].PodSelector.MatchLabels)
	assert.Len(t, harness.daemon.created, 1)
}

func TestMihomoConfigRefusesTheClusterAfterResolving(t *testing.T) {
	data, err := mihomoConfig("secret", []string{"10.96.0.0/12", testClusterRange})
	require.NoError(t, err)
	var config struct {
		Listeners []struct {
			Port  int `json:"port"`
			Users []struct {
				Username string `json:"username"`
				Password string `json:"password"`
			} `json:"users"`
		} `json:"listeners"`
		DNS struct {
			Nameserver []string `json:"nameserver"`
		} `json:"dns"`
		Rules []string `json:"rules"`
	}
	require.NoError(t, yaml.Unmarshal(data, &config))
	require.Len(t, config.Listeners, 1)
	assert.Equal(t, needyUpstreamPort, config.Listeners[0].Port)
	assert.Equal(t, needyUpstreamActorUser, config.Listeners[0].Users[0].Username)
	assert.Equal(t, "secret", config.Listeners[0].Users[0].Password)
	assert.NotEmpty(t, config.DNS.Nameserver)

	rules := config.Rules
	assert.Equal(t, "IP-CIDR,10.96.0.0/12,REJECT", rules[0], "the cluster's ranges come first")
	assert.Equal(t, 1, countOccurrences(rules, "IP-CIDR,"+testClusterRange+",REJECT"), "a cluster range that is also reserved appears once")
	assert.Contains(t, rules, "IP-CIDR,169.254.0.0/16,REJECT")
	for _, rule := range rules {
		assert.NotContains(t, rule, "no-resolve", "the address a name resolves to must be checked")
	}
	unresolved := slices.Index(rules, "NOT,((IP-CIDR,0.0.0.0/0)),REJECT")
	require.Positive(t, unresolved, "a name that does not resolve must not reach DIRECT")
	assert.Equal(t, "MATCH,DIRECT", rules[len(rules)-1])
	assert.Less(t, unresolved, len(rules)-1)
}

func countOccurrences(values []string, value string) int {
	count := 0
	for _, candidate := range values {
		if candidate == value {
			count++
		}
	}

	return count
}
