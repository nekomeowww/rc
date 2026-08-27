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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
)

const (
	testNamespace     = "development"
	testWorkspaceName = "coding"
	testStorageClass  = "clone-capable"
	testTrueValue     = "true"
	testAPINamespace  = "default"
	testRuntimeImage  = "workspace:test"
)

type recordingProcessRuntime struct {
	startedTarget  processruntime.Target
	startedRequest processruntime.StartRequest
	stoppedTarget  processruntime.Target
	stoppedID      string
	startState     processruntime.State
	inspectState   processruntime.State
	stopState      processruntime.State
}

func (processRuntime *recordingProcessRuntime) Start(_ context.Context, target processruntime.Target, request processruntime.StartRequest) (processruntime.State, error) {
	processRuntime.startedTarget = target
	processRuntime.startedRequest = request

	return processRuntime.startState, nil
}

func (processRuntime *recordingProcessRuntime) Inspect(context.Context, processruntime.Target, string) (processruntime.State, error) {
	return processRuntime.inspectState, nil
}

func (processRuntime *recordingProcessRuntime) Stop(_ context.Context, target processruntime.Target, id string) (processruntime.State, error) {
	processRuntime.stoppedTarget = target
	processRuntime.stoppedID = id

	return processRuntime.stopState, nil
}

func TestAgentProcessReconcileStartsCommandAtReadyWorkspace(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")

	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testNamespace},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DefaultWorkingDirectory: "/workspace/rc",
			Env:                     []corev1.EnvVar{{Name: "CI", Value: testTrueValue}},
		},
		Status: workspacesv1alpha1.WorkspaceStatus{
			RuntimePodName: testWorkspaceName,
			Conditions: []metav1.Condition{{
				Type: workspacesv1alpha1.WorkspaceConditionReady, Status: metav1.ConditionTrue, Reason: "WorkspaceReady",
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testNamespace, UID: types.UID("runtime-pod-uid")},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	process := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "codex-01k2example", Namespace: testNamespace, UID: types.UID("process-uid")},
		Spec: workspacesv1alpha1.AgentProcessSpec{
			TargetRef:    workspacesv1alpha1.AgentProcessTargetReference{Kind: workspacesv1alpha1.AgentProcessTargetWorkspace, Name: workspace.Name},
			Command:      []string{agentTypeCodex, "implement the task"},
			TTY:          true,
			DesiredState: workspacesv1alpha1.AgentProcessDesiredStateRunning,
			AgentType:    agentTypeCodex,
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(workspace, pod, process).
		WithObjects(workspace, pod, process).
		Build()
	runtimeClient := &recordingProcessRuntime{startState: processruntime.State{
		ID: process.Name, UID: string(process.UID), Phase: "Running", PID: 42,
	}}
	reconciler := &AgentProcessReconciler{Client: kubeClient, Scheme: scheme, Runtime: runtimeClient}
	key := types.NamespacedName{Name: process.Name, Namespace: process.Namespace}

	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "install AgentProcess finalizer")
	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "start Agent Process")
	assertions.Equal(process.Name, runtimeClient.startedRequest.ID, "use rc process ID")
	assertions.Equal(string(process.UID), runtimeClient.startedRequest.UID, "use UID as at-most-once key")
	assertions.Equal(process.Spec.Command, runtimeClient.startedRequest.Command, "preserve exact argv")
	assertions.Equal(workspace.Spec.DefaultWorkingDirectory, runtimeClient.startedRequest.WorkingDirectory, "use Workspace cwd")
	assertions.Equal(testTrueValue, runtimeClient.startedRequest.Environment["CI"], "include Workspace environment")
	assertions.Equal(workspace.Namespace, runtimeClient.startedTarget.Namespace, "target same namespace")
	assertions.Equal(workspace.Status.RuntimePodName, runtimeClient.startedTarget.Pod, "target original runtime Pod")

	persisted := new(workspacesv1alpha1.AgentProcess)
	requirements.NoError(kubeClient.Get(ctx, key, persisted), "get reconciled Agent Process")
	assertions.Equal(workspacesv1alpha1.AgentProcessPhaseRunning, persisted.Status.Phase, "publish Running")
	assertions.Equal(string(pod.UID), persisted.Status.RuntimePodUID, "bind process to original Pod UID")
	assertions.Equal(testWorkspaceName, persisted.Status.RuntimePodName, "publish runtime Pod name")
	assertions.NotNil(persisted.Status.StartedAt, "record start time")
	assertions.Equal(".rc/processes/codex-01k2example/transcript.log", persisted.Status.TranscriptPath, "publish transcript index")
}

func TestProcessCredentialProjectsFilesAndEnvsIndependently(t *testing.T) {
	t.Parallel()
	const credentialName = "tool-auth"
	const mountPath = "/home/agent/.tool/credentials.json"
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme), "register core API types")
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register config API types")
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")

	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testNamespace},
		Spec:       workspacesv1alpha1.WorkspaceSpec{CredentialRefs: []workspacesv1alpha1.LocalReference{{Name: credentialName}}},
	}
	credential := &configsv1alpha1.Credential{
		ObjectMeta: metav1.ObjectMeta{Name: credentialName, Namespace: testNamespace},
		Spec: configsv1alpha1.CredentialSpec{
			Type: configsv1alpha1.CredentialTypeProcess,
			Process: &configsv1alpha1.ProcessCredential{
				Files: []configsv1alpha1.CredentialFile{{
					DataRef: configsv1alpha1.SecretKeyReference{Name: "tool-auth-file", Key: "data"}, MountPath: mountPath,
				}},
				Envs: []configsv1alpha1.CredentialEnv{{Name: "TOOL_HOME", Value: "/home/agent/.tool"}},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tool-auth-file", Namespace: testNamespace},
		Data:       map[string][]byte{"data": []byte("raw\x00credential")},
	}
	process := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "tool-process", Namespace: testNamespace},
		Spec:       workspacesv1alpha1.AgentProcessSpec{CredentialRefs: []workspacesv1alpha1.LocalReference{{Name: credentialName}}},
	}
	reconciler := &AgentProcessReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, credential, secret).Build(),
		Scheme: scheme,
	}

	agentHome, files, err := reconciler.resolveProcessCredentials(ctx, workspace, process)
	require.NoError(t, err)
	assert.Empty(t, agentHome)
	assert.Equal(t, []byte("raw\x00credential"), files["credentials/"+credentialName+"/files/0"])
	projection, err := reconciler.resolveCredentialProjections(ctx, testNamespace, process.Spec.CredentialRefs)
	require.NoError(t, err)
	assert.Equal(t, []processruntime.CredentialMount{{Source: "credentials/" + credentialName + "/files/0", Target: mountPath}}, projection.mounts)
	assert.Equal(t, map[string]string{"TOOL_HOME": "/home/agent/.tool"}, projection.environment)
	assert.Empty(t, projection.sshConfigFragments)

	request, err := reconciler.processStartRequest(ctx, process, &resolvedProcessTarget{
		credentials:           files,
		mounts:                projection.mounts,
		credentialEnvironment: projection.environment,
	})
	require.NoError(t, err)
	assert.Equal(t, "/home/agent/.tool", request.Environment["TOOL_HOME"])
	assert.Equal(t, projection.mounts, request.CredentialMounts)
}

func TestSSHCredentialProjectsNativeConfiguration(t *testing.T) {
	t.Parallel()
	const credentialName = "github-ssh"
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme), "register core API types")
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register config API types")
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")

	credential := &configsv1alpha1.Credential{
		ObjectMeta: metav1.ObjectMeta{Name: credentialName, Namespace: testNamespace},
		Spec: configsv1alpha1.CredentialSpec{
			Type: configsv1alpha1.CredentialTypeSSHPrivateKey,
			SSHPrivateKey: &configsv1alpha1.SSHPrivateKeyCredential{
				PrivateKeyRef: configsv1alpha1.SecretKeyReference{Name: credentialName, Key: "ssh-privatekey"},
				KnownHostsRef: configsv1alpha1.SecretKeyReference{Name: credentialName, Key: "known_hosts"},
				Config:        "Host github.com\n  User git\n  IdentityFile ${identityFile}\n  UserKnownHostsFile ${knownHostsFile}\n  IdentitiesOnly yes\n",
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credentialName, Namespace: testNamespace},
		Data: map[string][]byte{
			"ssh-privatekey": []byte("private-key"),
			"known_hosts":    []byte("github.com ssh-ed25519 host-key"),
		},
	}
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testNamespace},
		Spec:       workspacesv1alpha1.WorkspaceSpec{CredentialRefs: []workspacesv1alpha1.LocalReference{{Name: credentialName}}},
	}
	process := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "ssh-process", Namespace: testNamespace},
		Spec:       workspacesv1alpha1.AgentProcessSpec{CredentialRefs: []workspacesv1alpha1.LocalReference{{Name: credentialName}}},
	}
	reconciler := &AgentProcessReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(credential, secret).Build(),
		Scheme: scheme,
	}

	_, files, err := reconciler.resolveProcessCredentials(ctx, workspace, process)
	require.NoError(t, err)
	assert.Equal(t, []byte("private-key"), files["credentials/"+credentialName+"/id"])
	assert.Equal(t, []byte("github.com ssh-ed25519 host-key"), files["credentials/"+credentialName+"/known_hosts"])
	projection, err := reconciler.resolveCredentialProjections(ctx, testNamespace, process.Spec.CredentialRefs)
	require.NoError(t, err)
	assert.Empty(t, projection.mounts)
	assert.Empty(t, projection.environment)
	assert.Equal(t, credential.Spec.SSHPrivateKey.Config, projection.sshConfigFragments[credentialName])

	request, err := reconciler.processStartRequest(ctx, process, &resolvedProcessTarget{
		credentials: files, sshConfigFragments: projection.sshConfigFragments,
	})
	require.NoError(t, err)
	assert.Equal(t, workspaceSSHConfigPath, request.SSHConfigPath)
	assert.Equal(t, projection.sshConfigFragments, request.SSHConfigFragments)
}

func TestRunningAgentProcessBecomesLostWhenOriginalPodDisappears(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	process := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "codex-lost", Namespace: testNamespace, UID: types.UID("process-uid")},
		Spec: workspacesv1alpha1.AgentProcessSpec{
			TargetRef: workspacesv1alpha1.AgentProcessTargetReference{Kind: workspacesv1alpha1.AgentProcessTargetWorkspace, Name: testWorkspaceName},
			Command:   []string{agentTypeCodex}, DesiredState: workspacesv1alpha1.AgentProcessDesiredStateRunning,
		},
		Status: workspacesv1alpha1.AgentProcessStatus{Phase: workspacesv1alpha1.AgentProcessPhaseRunning, RuntimePodName: testWorkspaceName, RuntimePodUID: "original-uid"},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(process).WithObjects(process).Build()
	reconciler := &AgentProcessReconciler{Client: kubeClient, Scheme: scheme, Runtime: &recordingProcessRuntime{}}
	key := types.NamespacedName{Name: process.Name, Namespace: process.Namespace}
	_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "install AgentProcess finalizer")
	_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	requirements.NoError(err, "reconcile missing original runtime")
	persisted := new(workspacesv1alpha1.AgentProcess)
	requirements.NoError(kubeClient.Get(context.Background(), key, persisted), "get lost process")
	requirements.Equal(workspacesv1alpha1.AgentProcessPhaseLost, persisted.Status.Phase, "never attach a replacement runtime")
}

func TestRuntimePodEventEnqueuesBoundActiveAgentProcesses(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "environment-editor", Namespace: testNamespace, UID: types.UID("replacement-pod-uid"),
		Labels: map[string]string{environmentManagedByLabel: "environment"},
	}}
	running := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "running-on-replaced-pod", Namespace: testNamespace},
		Status: workspacesv1alpha1.AgentProcessStatus{
			Phase: workspacesv1alpha1.AgentProcessPhaseRunning, RuntimePodName: pod.Name, RuntimePodUID: "original-pod-uid",
		},
	}
	starting := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "starting-on-runtime-pod", Namespace: testNamespace},
		Status: workspacesv1alpha1.AgentProcessStatus{
			Phase: workspacesv1alpha1.AgentProcessPhaseStarting, RuntimePodName: pod.Name, RuntimePodUID: string(pod.UID),
		},
	}
	terminal := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "completed-on-runtime-pod", Namespace: testNamespace},
		Status: workspacesv1alpha1.AgentProcessStatus{
			Phase: workspacesv1alpha1.AgentProcessPhaseSucceeded, RuntimePodName: pod.Name, RuntimePodUID: string(pod.UID),
		},
	}
	unrelated := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "running-elsewhere", Namespace: testNamespace},
		Status: workspacesv1alpha1.AgentProcessStatus{
			Phase: workspacesv1alpha1.AgentProcessPhaseRunning, RuntimePodName: "another-runtime", RuntimePodUID: "another-pod-uid",
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(running, starting, terminal, unrelated).
		Build()
	reconciler := &AgentProcessReconciler{Client: kubeClient, Scheme: scheme}

	requests := reconciler.agentProcessesForRuntimePod(context.Background(), pod)

	assertions.ElementsMatch([]reconcile.Request{
		{NamespacedName: client.ObjectKeyFromObject(running)},
		{NamespacedName: client.ObjectKeyFromObject(starting)},
	}, requests, "enqueue every active process bound to the changed runtime Pod name")
}

func TestDeletingActiveAgentProcessStopsOriginalRuntimeBeforeRemovingFinalizer(t *testing.T) {
	t.Parallel()

	for _, phase := range []workspacesv1alpha1.AgentProcessPhase{
		workspacesv1alpha1.AgentProcessPhaseStarting,
		workspacesv1alpha1.AgentProcessPhaseRunning,
	} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			requirements := require.New(t)
			ctx := context.Background()
			scheme := runtime.NewScheme()
			requirements.NoError(corev1.AddToScheme(scheme), "register core API types")
			requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")

			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: testWorkspaceName, Namespace: testNamespace, UID: types.UID("runtime-pod-uid"),
			}}
			process := &workspacesv1alpha1.AgentProcess{
				ObjectMeta: metav1.ObjectMeta{
					Name: "codex-delete-" + strings.ToLower(string(phase)), Namespace: testNamespace,
					UID: types.UID("process-uid"), Finalizers: []string{agentProcessFinalizer},
				},
				Spec: workspacesv1alpha1.AgentProcessSpec{
					TargetRef: workspacesv1alpha1.AgentProcessTargetReference{
						Kind: workspacesv1alpha1.AgentProcessTargetWorkspace, Name: testWorkspaceName,
					},
					Command: []string{agentTypeCodex},
				},
				Status: workspacesv1alpha1.AgentProcessStatus{
					Phase: phase, RuntimePodName: pod.Name, RuntimePodUID: string(pod.UID),
				},
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(process, pod).
				WithObjects(process, pod).
				Build()
			runtimeClient := &recordingProcessRuntime{stopState: processruntime.State{
				ID: process.Name, UID: string(process.UID), Phase: "Stopped",
			}}
			reconciler := &AgentProcessReconciler{Client: kubeClient, Scheme: scheme, Runtime: runtimeClient}
			key := client.ObjectKeyFromObject(process)

			requirements.NoError(kubeClient.Delete(ctx, process), "request direct AgentProcess deletion")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			requirements.NoError(err, "stop runtime while finalizing AgentProcess")
			requirements.Equal(process.Name, runtimeClient.stoppedID, "stop the UID-bound process")
			requirements.Equal(pod.Name, runtimeClient.stoppedTarget.Pod, "stop the original runtime Pod")
			err = kubeClient.Get(ctx, key, new(workspacesv1alpha1.AgentProcess))
			requirements.Error(err, "AgentProcess is deleted after runtime stops")
		})
	}
}
