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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/pkg/needy"
)

type needyHarness struct {
	t          *testing.T
	client     client.Client
	reconciler *NeedyReconciler
}

func newNeedyHarness(t *testing.T, config NeedyConfig, objects ...client.Object) *needyHarness {
	t.Helper()
	scheme := needyTestScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	harness := &needyHarness{
		t:      t,
		client: kube,
	}
	harness.reconciler = &NeedyReconciler{
		Client: kube,
		Scheme: scheme,
		Config: config,
	}

	return harness
}

func (harness *needyHarness) reconcile() reconcile.Result {
	harness.t.Helper()
	result, err := harness.reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: needyDaemonName}})
	require.NoError(harness.t, err)

	return result
}

func (harness *needyHarness) get(name string, object client.Object) error {
	return harness.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, object)
}

func TestNeedyDoesNothingInANamespaceWithoutNeedy(t *testing.T) {
	plain := executorWorkspace()
	plain.Spec.Needy = nil
	harness := newNeedyHarness(t, needyTestConfig(), plain)
	harness.reconcile()

	assert.True(t, apierrors.IsNotFound(harness.get(needyDaemonName, &appsv1.Deployment{})))
	assert.True(t, apierrors.IsNotFound(harness.get(needyFilesSecretName, &corev1.Secret{})))
}

func TestNeedyRunsADaemonForAnExecutorWorkspace(t *testing.T) {
	workspace := executorWorkspace()
	workspace.Spec.Needy.Executor.Labels = map[string]string{"tier": "build"}
	harness := newNeedyHarness(t, needyTestConfig(), workspace)
	harness.reconcile()

	var credential corev1.Secret
	require.NoError(t, harness.get(testExecutorName+"-needy-runner", &credential))
	value := string(credential.Data[needyCredentialKey])
	assert.True(t, needy.IsSecret(value))
	assert.Equal(t, testExecutorName, credential.OwnerReferences[0].Name)

	var runners corev1.Secret
	require.NoError(t, harness.get(needyFilesSecretName, &runners))
	var file struct {
		Runners []needy.StaticRunner `json:"runners"`
	}
	require.NoError(t, yaml.Unmarshal(runners.Data[needyRunnersFileKey], &file))
	assert.Equal(t, []needy.StaticRunner{
		{
			Name:           "ws-" + testExecutorName,
			CredentialHash: needy.Hash(value),
			Roles:          []needy.Role{needy.RoleExecutor},
			Labels: map[string]string{
				"tier":                                 "build",
				workspacesv1alpha1.NeedyWorkspaceLabel: testExecutorName,
			},
		},
	}, file.Runners)

	var deployment appsv1.Deployment
	require.NoError(t, harness.get(needyDaemonName, &deployment))
	assert.Equal(t, int32(1), *deployment.Spec.Replicas)
	assert.Equal(t, appsv1.RecreateDeploymentStrategyType, deployment.Spec.Strategy.Type)
	spec := deployment.Spec.Template.Spec
	assert.False(t, *spec.AutomountServiceAccountToken)
	assert.Empty(t, spec.NodeSelector)
	assert.NotNil(t, spec.Volumes[0].EmptyDir)
	command := strings.Join(spec.Containers[0].Command, " ")
	assert.Contains(t, command, " --disable-host-files")
	probe := spec.Containers[0].ReadinessProbe
	require.NotNil(t, probe.Exec, "the daemon's NetworkPolicy must not refuse its own readiness probe")
	assert.Equal(t, needy.DaemonCheckCommand(), probe.Exec.Command)
	assert.Contains(t, command, "--runners-file /etc/needy/files/runners.yaml")
	assert.Contains(t, command, "--clients-file /etc/needy/files/clients.yaml")
	filesVolume := spec.Volumes[slices.IndexFunc(spec.Volumes, func(volume corev1.Volume) bool {
		return volume.Name == needyFilesVolume
	})]
	assert.Equal(t, needyFilesSecretName, filesVolume.Secret.SecretName)
	assert.Empty(t, filesVolume.Secret.Items)
	require.NoError(t, harness.get(needyDaemonName, &corev1.Service{}))

	// The credential is stable across reconciles, and a prefixed credential
	// from before plain secrets is replaced.
	harness.reconcile()
	require.NoError(t, harness.get(testExecutorName+"-needy-runner", &credential))
	assert.Equal(t, value, string(credential.Data[needyCredentialKey]))
}

func TestNeedyPlacesTheDaemonOnKVMAndKeepsItsState(t *testing.T) {
	config := needyTestConfig()
	config.DaemonKVM = true
	config.DaemonStorageClass = "fast"
	config.GatewayBlockCIDRs = []string{"10.0.0.0/8"}
	harness := newNeedyHarness(t, config, executorWorkspace())
	harness.reconcile()

	var claim corev1.PersistentVolumeClaim
	require.NoError(t, harness.get(needyStateClaimName, &claim))
	assert.Equal(t, "fast", *claim.Spec.StorageClassName)
	var deployment appsv1.Deployment
	require.NoError(t, harness.get(needyDaemonName, &deployment))
	spec := deployment.Spec.Template.Spec
	assert.Equal(t, needyStateClaimName, spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	assert.Equal(t, map[string]string{testKVMResource: testTrueValue}, spec.NodeSelector)
	assert.Equal(t, []int64{testKVMGroup}, spec.SecurityContext.SupplementalGroups)
	assert.Contains(t, spec.Containers[0].Resources.Limits, corev1.ResourceName(testKVMResource))
	assert.Contains(t, strings.Join(spec.Containers[0].Command, " "), "--gateway-block-cidr 10.0.0.0/8")
}

func TestNeedyProvisionsTheManagerAsAStaticClient(t *testing.T) {
	harness := newNeedyHarness(t, needyTestConfig(), executorWorkspace())
	harness.reconcile()

	var token corev1.Secret
	require.NoError(t, harness.get(needyClientSecretName, &token))
	value := string(token.Data[needyClientTokenKey])
	assert.True(t, needy.IsSecret(value))

	var files corev1.Secret
	require.NoError(t, harness.get(needyFilesSecretName, &files))
	var clients struct {
		Clients []needy.StaticClient `json:"clients"`
	}
	require.NoError(t, yaml.Unmarshal(files.Data[needyClientsFileKey], &clients))
	assert.Equal(t, []needy.StaticClient{
		{
			Name:      needyControllerClientName,
			Access:    needy.AccessAdmin,
			TokenHash: needy.Hash(value),
		},
	}, clients.Clients)
	assert.NotContains(t, string(files.Data[needyClientsFileKey]), value, "the daemon's Secret holds only the hash")

	// The token is stable across reconciles.
	harness.reconcile()
	require.NoError(t, harness.get(needyClientSecretName, &token))
	assert.Equal(t, value, string(token.Data[needyClientTokenKey]))
}

func TestNeedyScalesTheDaemonToZeroWhenUnused(t *testing.T) {
	harness := newNeedyHarness(t, needyTestConfig(), executorWorkspace())
	harness.reconcile()

	var workspace workspacesv1alpha1.Workspace
	require.NoError(t, harness.get(testExecutorName, &workspace))
	workspace.Spec.Needy = nil
	require.NoError(t, harness.client.Update(context.Background(), &workspace))
	harness.reconcile()
	var deployment appsv1.Deployment
	require.NoError(t, harness.get(needyDaemonName, &deployment))
	assert.Equal(t, int32(0), *deployment.Spec.Replicas)

	// A live Actor needs the daemon.
	require.NoError(t, harness.client.Create(context.Background(), testActor()))
	harness.reconcile()
	require.NoError(t, harness.get(needyDaemonName, &deployment))
	assert.Equal(t, int32(1), *deployment.Spec.Replicas)

	// An ended Actor keeps it until its credential is revoked, then for a
	// grace in which its run can still be read.
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	harness.reconciler.Now = func() time.Time { return now }
	var actor workspacesv1alpha1.Actor
	require.NoError(t, harness.get(testActorName, &actor))
	completed := metav1.NewTime(now)
	actor.Status.Phase = workspacesv1alpha1.ActorPhaseSucceeded
	actor.Status.RunnerName = testActorRunner
	actor.Status.CompletedAt = &completed
	require.NoError(t, harness.client.Update(context.Background(), &actor))
	harness.reconcile()
	require.NoError(t, harness.get(needyDaemonName, &deployment))
	assert.Equal(t, int32(1), *deployment.Spec.Replicas, "the credential is not revoked yet")

	require.NoError(t, harness.get(testActorName, &actor))
	actor.Annotations = map[string]string{actorCredentialRevokedAnnotation: testTrueValue}
	require.NoError(t, harness.client.Update(context.Background(), &actor))
	result := harness.reconcile()
	require.NoError(t, harness.get(needyDaemonName, &deployment))
	assert.Equal(t, int32(1), *deployment.Spec.Replicas, "the run can still be read")
	assert.Equal(t, needyDaemonGrace+time.Second, result.RequeueAfter)

	now = now.Add(needyDaemonGrace)
	harness.reconcile()
	require.NoError(t, harness.get(needyDaemonName, &deployment))
	assert.Equal(t, int32(0), *deployment.Spec.Replicas)
}

func TestNeedyRunnerNamesAreDNSLabels(t *testing.T) {
	assert.Equal(t, "ws-"+testExecutorName, needyWorkspaceRunnerName(testExecutorName))
	long := needyWorkspaceRunnerName(strings.Repeat("a", 80))
	assert.Len(t, long, validation.DNS1123LabelMaxLength)
	assert.Empty(t, validation.IsDNS1123Label(long))
	assert.Equal(t, long, needyWorkspaceRunnerName(strings.Repeat("a", 80)), "the name is stable")
	assert.NotEqual(t, long, needyWorkspaceRunnerName(strings.Repeat("a", 79)))
}

func TestNeedyReplacesPrefixedSecrets(t *testing.T) {
	prefixed := "needy-runner.ws-" + testExecutorName + "." + strings.Repeat("a", 64)
	old := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: needyExecutorSecretName(testExecutorName), Namespace: testNamespace},
		Data:       map[string][]byte{needyCredentialKey: []byte(prefixed)},
	}
	harness := newNeedyHarness(t, needyTestConfig(), executorWorkspace(), old)
	harness.reconcile()

	var credential corev1.Secret
	require.NoError(t, harness.get(needyExecutorSecretName(testExecutorName), &credential))
	value := string(credential.Data[needyCredentialKey])
	assert.NotEqual(t, prefixed, value)
	assert.True(t, needy.IsSecret(value))
}
