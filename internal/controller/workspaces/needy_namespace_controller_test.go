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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/needy"
)

type needyHarness struct {
	t          *testing.T
	client     client.Client
	reconciler *NeedyReconciler
}

func newNeedyHarness(t *testing.T, config needy.Config, objects ...client.Object) *needyHarness {
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
	result, err := harness.reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: needy.DaemonName}})
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

	assert.True(t, apierrors.IsNotFound(harness.get(needy.DaemonName, &appsv1.Deployment{})))
	assert.True(t, apierrors.IsNotFound(harness.get(needy.FilesSecretName, &corev1.Secret{})))
}

func TestNeedyRunsADaemonForAnExecutorWorkspace(t *testing.T) {
	workspace := executorWorkspace()
	workspace.Spec.Needy.Executor.Labels = map[string]string{"tier": "build"}
	harness := newNeedyHarness(t, needyTestConfig(), workspace)
	harness.reconcile()

	var credential corev1.Secret
	require.NoError(t, harness.get(testExecutorName+"-needy-runner", &credential))
	value := string(credential.Data[needy.CredentialKey])
	assert.True(t, needy.CredentialMatchesRunner(value, "ws-"+testExecutorName))
	assert.Equal(t, testExecutorName, credential.OwnerReferences[0].Name)

	var runners corev1.Secret
	require.NoError(t, harness.get(needy.FilesSecretName, &runners))
	var file struct {
		Runners []needy.StaticRunner `json:"runners"`
	}
	require.NoError(t, yaml.Unmarshal(runners.Data[needy.RunnersFileKey], &file))
	assert.Equal(t, []needy.StaticRunner{
		{
			Name:           "ws-" + testExecutorName,
			CredentialHash: needy.CredentialHash(value),
			Roles:          []string{"executor"},
			Labels: map[string]string{
				"tier":               "build",
				needy.WorkspaceLabel: testExecutorName,
			},
		},
	}, file.Runners)

	var deployment appsv1.Deployment
	require.NoError(t, harness.get(needy.DaemonName, &deployment))
	assert.Equal(t, int32(1), *deployment.Spec.Replicas)
	assert.Equal(t, appsv1.RecreateDeploymentStrategyType, deployment.Spec.Strategy.Type)
	spec := deployment.Spec.Template.Spec
	assert.False(t, *spec.AutomountServiceAccountToken)
	assert.Empty(t, spec.NodeSelector)
	assert.NotNil(t, spec.Volumes[0].EmptyDir)
	command := strings.Join(spec.Containers[0].Command, " ")
	assert.Contains(t, command, "'--disable-host-files'")
	probe := spec.Containers[0].ReadinessProbe
	require.NotNil(t, probe.Exec, "the daemon's NetworkPolicy must not refuse its own readiness probe")
	assert.Equal(t, needy.DaemonCheckCommand(), probe.Exec.Command)
	assert.Contains(t, command, "'--runners-file' '/etc/needy/files/runners.yaml'")
	assert.Contains(t, command, "'--clients-file' '/etc/needy/files/clients.yaml'")
	filesVolume := spec.Volumes[slices.IndexFunc(spec.Volumes, func(volume corev1.Volume) bool {
		return volume.Name == needyFilesVolume
	})]
	assert.Equal(t, needy.FilesSecretName, filesVolume.Secret.SecretName)
	assert.Empty(t, filesVolume.Secret.Items)
	require.NoError(t, harness.get(needy.DaemonName, &corev1.Service{}))

	// The credential is stable across reconciles.
	harness.reconcile()
	require.NoError(t, harness.get(testExecutorName+"-needy-runner", &credential))
	assert.Equal(t, value, string(credential.Data[needy.CredentialKey]))
}

func TestNeedyPlacesTheDaemonOnKVMAndKeepsItsState(t *testing.T) {
	config := needyTestConfig()
	config.DaemonKVM = true
	config.DaemonStorageClass = "fast"
	config.GatewayBlockCIDRs = []string{"10.0.0.0/8"}
	harness := newNeedyHarness(t, config, executorWorkspace())
	harness.reconcile()

	var claim corev1.PersistentVolumeClaim
	require.NoError(t, harness.get(needy.StateClaimName, &claim))
	assert.Equal(t, "fast", *claim.Spec.StorageClassName)
	var deployment appsv1.Deployment
	require.NoError(t, harness.get(needy.DaemonName, &deployment))
	spec := deployment.Spec.Template.Spec
	assert.Equal(t, needy.StateClaimName, spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	assert.Equal(t, map[string]string{testKVMResource: testTrueValue}, spec.NodeSelector)
	assert.Equal(t, []int64{testKVMGroup}, spec.SecurityContext.SupplementalGroups)
	assert.Contains(t, spec.Containers[0].Resources.Limits, corev1.ResourceName(testKVMResource))
	assert.Contains(t, strings.Join(spec.Containers[0].Command, " "), "'--gateway-block-cidr' '10.0.0.0/8'")
}

func TestNeedyProvisionsTheManagerAsAStaticClient(t *testing.T) {
	harness := newNeedyHarness(t, needyTestConfig(), executorWorkspace())
	harness.reconcile()

	var token corev1.Secret
	require.NoError(t, harness.get(needy.ClientSecretName, &token))
	value := string(token.Data[needy.ClientTokenKey])
	assert.True(t, needy.TokenMatchesClient(value, needy.ControllerClientName))

	var files corev1.Secret
	require.NoError(t, harness.get(needy.FilesSecretName, &files))
	var clients struct {
		Clients []needy.StaticClient `json:"clients"`
	}
	require.NoError(t, yaml.Unmarshal(files.Data[needy.ClientsFileKey], &clients))
	assert.Equal(t, []needy.StaticClient{
		{
			Name:      needy.ControllerClientName,
			Access:    "admin",
			TokenHash: needy.CredentialHash(value),
		},
	}, clients.Clients)
	assert.NotContains(t, string(files.Data[needy.ClientsFileKey]), value, "the daemon's Secret holds only the hash")

	// The token is stable across reconciles.
	harness.reconcile()
	require.NoError(t, harness.get(needy.ClientSecretName, &token))
	assert.Equal(t, value, string(token.Data[needy.ClientTokenKey]))
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
	require.NoError(t, harness.get(needy.DaemonName, &deployment))
	assert.Equal(t, int32(0), *deployment.Spec.Replicas)

	// A live Actor needs the daemon.
	require.NoError(t, harness.client.Create(context.Background(), testActor()))
	harness.reconcile()
	require.NoError(t, harness.get(needy.DaemonName, &deployment))
	assert.Equal(t, int32(1), *deployment.Spec.Replicas)
}

func TestWorkspaceNeedyExecutorService(t *testing.T) {
	workspace := serviceWorkspace()
	workspace.Spec.Needy = &workspacesv1alpha1.WorkspaceNeedy{Executor: &workspacesv1alpha1.WorkspaceNeedyExecutor{}}
	services, environment, reason, _ := compileWorkspaceServices(workspace)
	require.Empty(t, reason)
	require.Len(t, services, 2)
	service := services[1]
	assert.Equal(t, needyExecutorServiceName, service.Name)
	assert.Equal(t, "/opt/needy/bin/needy runner serve --address tcp://needy-daemon."+testNamespace+".svc:7400 "+
		"--credential-env "+needyCredentialVariable+" --roles executor --activity-hook /opt/rc/bin/rc-kube process touch",
		strings.Join(service.Command, " "))
	var credential *corev1.EnvVar
	for i := range environment {
		if source := environment[i].ValueFrom; source != nil && source.SecretKeyRef != nil && source.SecretKeyRef.Name == workspace.Name+"-needy-runner" {
			credential = &environment[i]
		}
	}
	require.NotNil(t, credential)
	assert.Equal(t, needy.CredentialKey, credential.ValueFrom.SecretKeyRef.Key)

	workspace.Spec.Services[0].Name = needyExecutorServiceName
	_, _, reason, _ = compileWorkspaceServices(workspace)
	assert.Equal(t, reasonInvalidServiceEnvironment, reason)
}
