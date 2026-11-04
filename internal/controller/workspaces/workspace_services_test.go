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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/rcplatform"
)

const (
	testServiceName     = "agent"
	testServiceSecret   = "agent-token"
	testServiceCommand  = "device-agent"
	testServiceVariable = "TOKEN"
	testModeVariable    = "MODE"
	testServiceArgument = "--gateway"
)

func serviceWorkspace() *workspacesv1alpha1.Workspace {
	return &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "device", Namespace: testNamespace, UID: "device-uid"},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DefaultWorkingDirectory: "/workspace/app",
			Env: []corev1.EnvVar{
				{
					Name:  testModeVariable,
					Value: "inherited",
				},
				{
					Name: testServiceVariable,
					ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: testServiceSecret},
						Key:                  "token",
					}},
				},
			},
			Services: []workspacesv1alpha1.WorkspaceService{
				{
					Name:    testServiceName,
					Command: []string{testServiceCommand, testServiceArgument},
					Env: []corev1.EnvVar{
						{
							Name:  testModeVariable,
							Value: "service",
						},
						{
							Name: "GATEWAY",
							ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "gateway"},
								Key:                  "url",
							}},
						},
					},
				},
			},
		},
	}
}

func TestCompileWorkspaceServicesInheritsWorkspaceEnvironment(t *testing.T) {
	t.Parallel()
	workspace := serviceWorkspace()

	services, environment, reason, _ := compileWorkspaceServices(workspace)

	require.Empty(t, reason)
	require.Len(t, services, 1)
	assert.Equal(t, processruntime.Service{
		Name:             testServiceName,
		Command:          []string{testServiceCommand, testServiceArgument},
		WorkingDirectory: "/workspace/app",
		Environment:      map[string]string{testModeVariable: "service"},
		EnvironmentFrom: map[string]string{
			"GATEWAY":           processruntime.ServiceEnvironmentPrefix + "0",
			testServiceVariable: processruntime.ServiceEnvironmentPrefix + "1",
		},
	}, services[0], "the service env overrides the Workspace env, as a WorkspaceExec env does")
	require.Len(t, environment, 2)
	assert.Equal(t, processruntime.ServiceEnvironmentPrefix+"1", environment[1].Name)
	assert.Equal(t, testServiceSecret, environment[1].ValueFrom.SecretKeyRef.Name, "Kubernetes reads the Secret; rc never copies its value")

	again, _, _, _ := compileWorkspaceServices(workspace)
	assert.Equal(t, services, again, "the compiled form is deterministic because it is hashed")
}

func TestCompileWorkspaceServicesRejectsFieldReferences(t *testing.T) {
	t.Parallel()
	workspace := serviceWorkspace()
	workspace.Spec.Env = append(workspace.Spec.Env, corev1.EnvVar{
		Name:      "NODE",
		ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
	})

	_, _, reason, message := compileWorkspaceServices(workspace)

	assert.Equal(t, reasonInvalidServiceEnvironment, reason, "a WorkspaceExec cannot read field references either")
	assert.Contains(t, message, "NODE")
}

func TestResolveWorkspaceServicesReportsMissingReferences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: testNamespace}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	reconciler := &WorkspaceReconciler{Client: kubeClient, APIReader: kubeClient}
	resolved := &resolvedWorkspace{}

	reason, message, err := reconciler.resolveWorkspaceServices(ctx, serviceWorkspace(), resolved)
	require.NoError(t, err)
	assert.Equal(t, "SecretNotFound", reason, "report a missing Secret on the Workspace instead of a stuck container")
	assert.Contains(t, message, testServiceSecret)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testServiceSecret, Namespace: testNamespace}}
	require.NoError(t, kubeClient.Create(ctx, secret))
	reason, _, err = reconciler.resolveWorkspaceServices(ctx, serviceWorkspace(), resolved)
	require.NoError(t, err)
	assert.Empty(t, reason)
	assert.Len(t, resolved.services, 1)
	assert.Len(t, resolved.serviceEnvironment, 2)
}

func TestWorkspaceTopologyTracksServices(t *testing.T) {
	t.Parallel()
	platform, err := rcplatform.Resolve(rcplatform.Target{OS: corev1.Linux})
	require.NoError(t, err)
	workspace := serviceWorkspace()
	workspace.Spec.Services = nil
	plain := &resolvedWorkspace{runtime: platform, image: testLinuxImage}
	withoutServices, err := workspaceTopologyHash(workspace, plain)
	require.NoError(t, err)
	workspace.Spec.Env = append(workspace.Spec.Env, corev1.EnvVar{Name: "EXTRA", Value: "1"})
	changedEnv, err := workspaceTopologyHash(workspace, plain)
	require.NoError(t, err)
	assert.Equal(t, withoutServices, changedEnv, "without services, env is read per execution and needs no new runtime")

	workspace = serviceWorkspace()
	services, environment, _, _ := compileWorkspaceServices(workspace)
	withServices := &resolvedWorkspace{runtime: platform, image: testLinuxImage, services: services, serviceEnvironment: environment}
	first, err := workspaceTopologyHash(workspace, withServices)
	require.NoError(t, err)
	assert.NotEqual(t, withoutServices, first, "adding a service takes a new runtime")

	workspace.Spec.Env = append(workspace.Spec.Env, corev1.EnvVar{Name: "EXTRA", Value: "1"})
	services, environment, _, _ = compileWorkspaceServices(workspace)
	withServices = &resolvedWorkspace{runtime: platform, image: testLinuxImage, services: services, serviceEnvironment: environment}
	second, err := workspaceTopologyHash(workspace, withServices)
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "a service sees the Workspace env, so a change takes a new runtime")

	pod, err := workspaceRuntimePod(workspace, withServices)
	require.NoError(t, err)
	assert.Equal(t, second, pod.Annotations[workspaceTopologyAnnotation])
	assert.Contains(t, pod.Spec.Containers[0].Args, "--services")
	assert.Equal(t, environment, pod.Spec.Containers[0].Env)
}

func TestRetentionSuspendsIdleWorkspaceWithRunningServices(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace, pod := runningWorkspace("device", time.Now().Add(-2*time.Hour))
	workspace.Spec.IdleTimeout = &metav1.Duration{Duration: time.Hour}
	workspace.Spec.MaxRunDuration = &metav1.Duration{Duration: 8 * time.Hour}
	workspace.Spec.Services = serviceWorkspace().Spec.Services
	kubeClient := newRetentionTestClient(t, workspace, pod)
	// A running service that reported no work leaves the supervisor without
	// recorded activity: its start and restarts never touch.
	activity := &stubActivityReader{}
	reconciler := &WorkspaceRetentionReconciler{Client: kubeClient, APIReader: kubeClient, Activity: activity}

	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	require.NoError(t, err)

	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted))
	assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateSuspended, persisted.Spec.DesiredState, "services do not keep an idle Workspace running")
	require.NotNil(t, persisted.Status.Lifecycle)
	assert.Equal(t, workspacesv1alpha1.WorkspaceAutomaticSuspendIdle, persisted.Status.Lifecycle.LastAutomaticSuspendReason)
	assert.Zero(t, persisted.Status.Lifecycle.ActiveExecutions, "services are not executions")
}
