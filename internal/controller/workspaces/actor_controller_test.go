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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	workspacesv1alpha1 "github.com/nekomeowww/rc/apis/kubernetes/workspaces/v1alpha1"
	needyv1 "github.com/nekomeowww/rc/apis/sdk/go/needy/v1"
	"github.com/nekomeowww/rc/pkg/needy"
)

const (
	testActorName   = "review"
	testActorUID    = "0123abcd-89ab-cdef-0123-456789abcdef"
	testActorRunner = "actor-0123abcd89ab"
	testRunID       = "run-1"
)

// fakeDaemon records calls and serves runs from a map.
type fakeDaemon struct {
	runs        map[string]needy.Run
	created     []needy.RunSpec
	minted      []string
	revoked     []string
	stopped     []string
	placement   error
	credentials map[string]string
	offline     map[string]bool
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{
		runs:        make(map[string]needy.Run),
		credentials: make(map[string]string),
		offline:     make(map[string]bool),
	}
}

func (daemon *fakeDaemon) Dial(context.Context, string, string) (needy.Daemon, error) {
	return daemon, nil
}

func (daemon *fakeDaemon) CheckPlacement(context.Context, string) error {
	return daemon.placement
}

func (daemon *fakeDaemon) IsActorRunnerConnected(_ context.Context, name string) (bool, error) {
	return !daemon.offline[name], nil
}

func (daemon *fakeDaemon) MintRunnerCredential(_ context.Context, name string, _ time.Duration) (string, error) {
	daemon.minted = append(daemon.minted, name)
	// The daemon mints credentials in its prefixed form.
	secret, err := needy.NewSecret()
	credential := "needy-runner." + name + "." + secret
	daemon.credentials[name] = credential

	return credential, err
}

func (daemon *fakeDaemon) RevokeRunnerCredential(_ context.Context, name string) error {
	daemon.revoked = append(daemon.revoked, name)

	return nil
}

func (daemon *fakeDaemon) CreateRun(_ context.Context, spec needy.RunSpec) (needy.Run, error) {
	daemon.created = append(daemon.created, spec)
	run := needy.Run{
		ID:         testRunID,
		State:      needyv1.RunState_RUN_STATE_RUNNING,
		ActorState: needyv1.ActorState_ACTOR_STATE_STARTING,
	}
	daemon.runs[run.ID] = run
	daemon.runs[spec.Name] = run

	return run, nil
}

func (daemon *fakeDaemon) GetRun(_ context.Context, id string) (needy.Run, error) {
	run, found := daemon.runs[id]
	if !found {
		return needy.Run{}, needy.ErrNotFound
	}

	return run, nil
}

func (daemon *fakeDaemon) StopRun(_ context.Context, id string, _ time.Duration) error {
	daemon.stopped = append(daemon.stopped, id)

	return nil
}

func (*fakeDaemon) Close() error {
	return nil
}

func testActor() *workspacesv1alpha1.Actor {
	return &workspacesv1alpha1.Actor{
		ObjectMeta: metav1.ObjectMeta{Name: testActorName, Namespace: testNamespace, UID: testActorUID},
		Spec: workspacesv1alpha1.ActorSpec{
			Profile: workspacesv1alpha1.ActorProfile{Inline: "extends: builtin:opencode\ncommands:\n  - { match: \"**\", to: ws }\n"},
			Executors: []workspacesv1alpha1.ActorExecutor{
				{
					Name:         "ws",
					WorkspaceRef: corev1.LocalObjectReference{Name: testExecutorName},
				},
			},
			Terminal:       true,
			MaxRunDuration: &metav1.Duration{Duration: time.Hour},
		},
	}
}

func availableDaemon() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: needyDaemonName, Namespace: testNamespace},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
}

func daemonClientSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: needyClientSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{needyClientTokenKey: []byte("needy-client.rc.token")},
	}
}

type actorHarness struct {
	t          *testing.T
	client     client.Client
	daemon     *fakeDaemon
	reconciler *ActorReconciler
	now        time.Time
}

func newActorHarness(t *testing.T, objects ...client.Object) *actorHarness {
	t.Helper()

	return newActorHarnessFor(t, testActor(), objects...)
}

func newActorHarnessFor(t *testing.T, actor *workspacesv1alpha1.Actor, objects ...client.Object) *actorHarness {
	t.Helper()
	scheme := needyTestScheme(t)
	objects = append(objects, actor)
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&workspacesv1alpha1.Actor{}, &corev1.Pod{}, &appsv1.Deployment{}).
		WithObjects(objects...).Build()
	harness := &actorHarness{
		t:      t,
		client: kube,
		daemon: newFakeDaemon(),
		now:    time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
	}
	harness.reconciler = &ActorReconciler{
		Client: kube,
		Scheme: scheme,
		Config: needyTestConfig(),
		Dialer: harness.daemon,
		Now: func() time.Time {
			return harness.now
		},
	}

	return harness
}

func (harness *actorHarness) reconcile() reconcile.Result {
	harness.t.Helper()
	result, err := harness.reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testActorName}})
	require.NoError(harness.t, err)

	return result
}

func (harness *actorHarness) actor() *workspacesv1alpha1.Actor {
	harness.t.Helper()
	var actor workspacesv1alpha1.Actor
	require.NoError(harness.t, harness.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testActorName}, &actor))

	return &actor
}

func (harness *actorHarness) pod() *corev1.Pod {
	harness.t.Helper()
	var pod corev1.Pod
	require.NoError(harness.t, harness.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testActorName + "-runner"}, &pod))

	return &pod
}

func TestActorWaitsForTheDaemon(t *testing.T) {
	// The token exists before the daemon is available.
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret())
	result := harness.reconcile()

	actor := harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorPhasePending, actor.Status.Phase)
	assert.Equal(t, workspacesv1alpha1.ActorReasonWaitingForDaemon, actor.Status.Conditions[0].Reason)
	assert.Contains(t, actor.Finalizers, actorFinalizer)
	assert.Positive(t, result.RequeueAfter)
	assert.Empty(t, harness.daemon.minted)
}

func TestActorWaitsForItsExecutorWorkspace(t *testing.T) {
	plain := executorWorkspace()
	plain.Spec.Needy = nil
	harness := newActorHarness(t, plain, daemonClientSecret(), availableDaemon())
	harness.reconcile()

	actor := harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorPhasePending, actor.Status.Phase)
	assert.Equal(t, workspacesv1alpha1.ActorReasonExecutorNotReady, actor.Status.Conditions[0].Reason)
	assert.Empty(t, harness.daemon.minted)
}

func TestActorStartsARunOnItsOwnRunner(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.reconcile()

	actor := harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorPhaseRunning, actor.Status.Phase)
	assert.Equal(t, testRunID, actor.Status.RunID)
	assert.Equal(t, testActorRunner, actor.Status.RunnerName)
	assert.Equal(t, []string{testActorRunner}, harness.daemon.minted)

	require.Len(t, harness.daemon.created, 1)
	created := harness.daemon.created[0]
	assert.True(t, created.Terminal)
	assert.Equal(t, "actor:"+testActorName, created.ProfileName)
	assert.True(t, strings.HasPrefix(created.Name, testActorName+"-"))
	var profile map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(created.ProfileYAML), &profile))
	assert.Equal(t, testActorRunner, profile["actor"].(map[string]any)["runner"])
	assert.Equal(t, "ws-"+testExecutorName, profile["executors"].(map[string]any)["ws"].(map[string]any)["runner"])

	var credential corev1.Secret
	require.NoError(t, harness.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testActorName + "-needy-runner"}, &credential))
	assert.Equal(t, harness.daemon.credentials[testActorRunner], string(credential.Data[needyCredentialKey]))
	assert.Equal(t, testActorName, credential.OwnerReferences[0].Name)

	spec := harness.pod().Spec
	assert.Equal(t, corev1.RestartPolicyNever, spec.RestartPolicy)
	assert.False(t, *spec.AutomountServiceAccountToken)
	assert.Equal(t, []int64{testKVMGroup}, spec.SecurityContext.SupplementalGroups)
	assert.Equal(t, map[string]string{testKVMResource: testTrueValue}, spec.NodeSelector)
	assert.Len(t, spec.Tolerations, 1)
	assert.Equal(t, int64((time.Hour + actorStartGrace).Seconds()), *spec.ActiveDeadlineSeconds)
	container := spec.Containers[0]
	assert.Equal(t, testNeedyImage, container.Image)
	assert.Equal(t, resource.MustParse("1"), container.Resources.Limits[testKVMResource])
	command := strings.Join(container.Command, " ")
	assert.Contains(t, command, "--roles actor --ephemeral")
	assert.Contains(t, command, "--gateway tcp://needy-daemon."+testNamespace+".svc:7401")
}

const (
	testEnvSource     = "lobehub"
	testEnvGatewayURL = "https://gateway.example.com"
	testEnvGatewayKey = "gateway-url"
)

func envActor() *workspacesv1alpha1.Actor {
	actor := testActor()
	actor.Spec.Profile.Inline = "extends: builtin:opencode\nactor:\n  env:\n    KEPT: profile\n    LOBEHUB_SERVER: https://profile.example.com\ncommands:\n  - { match: \"**\", to: ws }\n"
	actor.Spec.Env = []corev1.EnvVar{
		{
			Name:  "LOBEHUB_SERVER",
			Value: "https://lobehub.example.com",
		},
		{
			Name: "LOBEHUB_CLI_API_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: testEnvSource},
					Key:                  "api-key",
				},
			},
		},
		{
			Name: "LOBEHUB_DEVICE_GATEWAY_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: testEnvSource},
					Key:                  testEnvGatewayKey,
				},
			},
		},
		{
			Name: "OPTIONAL",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "absent"},
					Key:                  "absent-key",
					Optional:             new(true),
				},
			},
		},
	}

	return actor
}

func TestActorPassesItsEnvToTheRun(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvSource, Namespace: testNamespace},
		Data:       map[string][]byte{"api-key": []byte("key")},
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvSource, Namespace: testNamespace},
		Data:       map[string]string{testEnvGatewayKey: testEnvGatewayURL},
	}
	harness := newActorHarnessFor(t, envActor(), executorWorkspace(), daemonClientSecret(), availableDaemon(), secret, configMap)
	harness.reconcile()

	require.Len(t, harness.daemon.created, 1)
	var profile map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(harness.daemon.created[0].ProfileYAML), &profile))
	assert.Equal(t, map[string]any{
		"KEPT":                       "profile",
		"LOBEHUB_SERVER":             "https://lobehub.example.com",
		"LOBEHUB_CLI_API_KEY":        "key",
		"LOBEHUB_DEVICE_GATEWAY_URL": testEnvGatewayURL,
	}, profile["actor"].(map[string]any)["env"])
	for _, container := range harness.pod().Spec.Containers {
		for _, variable := range container.Env {
			assert.NotEqual(t, "LOBEHUB_CLI_API_KEY", variable.Name, "the runner Pod does not get the actor's env")
		}
	}
}

func TestActorWaitsForItsEnvSecret(t *testing.T) {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvSource, Namespace: testNamespace},
		Data:       map[string]string{testEnvGatewayKey: testEnvGatewayURL},
	}
	harness := newActorHarnessFor(t, envActor(), executorWorkspace(), daemonClientSecret(), availableDaemon(), configMap)
	result := harness.reconcile()

	actor := harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorPhasePending, actor.Status.Phase)
	assert.Equal(t, workspacesv1alpha1.ActorReasonEnvNotReady, actor.Status.Conditions[0].Reason)
	assert.Contains(t, actor.Status.Conditions[0].Message, "Secret lobehub of env LOBEHUB_CLI_API_KEY does not exist")
	assert.Positive(t, result.RequeueAfter)
	assert.Empty(t, harness.daemon.minted, "nothing starts before the env is readable")
	assert.Empty(t, harness.daemon.created)
}

func TestActorWaitsForAMissingEnvKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvSource, Namespace: testNamespace},
		Data:       map[string][]byte{"unrelated": []byte("unused")},
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvSource, Namespace: testNamespace},
		Data:       map[string]string{testEnvGatewayKey: testEnvGatewayURL},
	}
	harness := newActorHarnessFor(t, envActor(), executorWorkspace(), daemonClientSecret(), availableDaemon(), secret, configMap)
	harness.reconcile()

	actor := harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorReasonEnvNotReady, actor.Status.Conditions[0].Reason)
	assert.Contains(t, actor.Status.Conditions[0].Message, "Secret lobehub has no key api-key")
	assert.Empty(t, harness.daemon.created)
}

func TestActorShapesItsRunnerPod(t *testing.T) {
	actor := testActor()
	actor.Spec.Pod = &workspacesv1alpha1.ActorPod{
		Metadata: &workspacesv1alpha1.WorkspacePodMetadata{
			Labels: map[string]string{
				"tenant.example.com/id": "t1",
				appNameLabel:            "not-needy",
			},
			Annotations: map[string]string{
				"example.com/cost": "actors",
				testDoNotDisrupt:   "false",
			},
		},
		NodeSelector: map[string]string{
			corev1.LabelTopologyZone: "zone-a",
			testKVMResource:          "false",
		},
		Tolerations: []corev1.Toleration{
			{
				Key:      "example.com/tenant",
				Operator: corev1.TolerationOpExists,
			},
		},
		Affinity: &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{},
		},
		PriorityClassName: "actors",
		ImagePullSecrets: []corev1.LocalObjectReference{
			{Name: "registry"},
		},
	}
	harness := newActorHarnessFor(t, actor, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.reconcile()

	pod := harness.pod()
	assert.Equal(t, "t1", pod.Labels["tenant.example.com/id"])
	assert.Equal(t, needyAppName, pod.Labels[appNameLabel], "rc's labels win")
	assert.Equal(t, needyComponentActor, pod.Labels[workspacesv1alpha1.NeedyComponentLabel])
	assert.Equal(t, "actors", pod.Annotations["example.com/cost"])
	assert.Equal(t, annotationTrue, pod.Annotations[testDoNotDisrupt], "rc's annotations win")
	assert.Equal(t, map[string]string{
		corev1.LabelTopologyZone: "zone-a",
		testKVMResource:          testTrueValue,
	}, pod.Spec.NodeSelector, "the manager's KVM selector wins")
	require.Len(t, pod.Spec.Tolerations, 2)
	assert.Equal(t, testKVMResource, pod.Spec.Tolerations[0].Key)
	assert.Equal(t, "example.com/tenant", pod.Spec.Tolerations[1].Key)
	assert.NotNil(t, pod.Spec.Affinity.PodAntiAffinity)
	assert.Equal(t, "actors", pod.Spec.PriorityClassName)
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "registry"}}, pod.Spec.ImagePullSecrets)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)
	assert.Equal(t, map[string]string{testKVMResource: testTrueValue}, map[string]string(harness.reconciler.Config.KVMNodeSelector), "the manager's selector is not changed")
}

func TestActorRefusesInvalidPodMetadata(t *testing.T) {
	actor := testActor()
	actor.Spec.Pod = &workspacesv1alpha1.ActorPod{
		Metadata: &workspacesv1alpha1.WorkspacePodMetadata{
			Labels: map[string]string{"bad key!": "value"},
		},
	}
	harness := newActorHarnessFor(t, actor, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.reconcile()

	status := harness.actor().Status
	assert.Equal(t, workspacesv1alpha1.ActorPhaseFailed, status.Phase)
	assert.Equal(t, workspacesv1alpha1.ActorReasonInvalidPod, status.Conditions[0].Reason)
	assert.Contains(t, status.Conditions[0].Message, "spec.pod.metadata.labels")
	assert.Empty(t, harness.daemon.minted, "no credential is minted for a Pod that cannot be created")
}

func TestActorWaitsForItsRunnerBeforeCheckingPlacement(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.daemon.offline[testActorRunner] = true
	harness.daemon.placement = assert.AnError
	result := harness.reconcile()

	actor := harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorPhaseStarting, actor.Status.Phase)
	assert.Equal(t, workspacesv1alpha1.ActorReasonWaitingForRunner, actor.Status.Conditions[0].Reason)
	assert.Positive(t, result.RequeueAfter)
	assert.Empty(t, harness.daemon.created)

	pod := harness.pod()
	pod.Status.Phase = corev1.PodFailed
	require.NoError(t, harness.client.Status().Update(context.Background(), pod))
	harness.reconcile()
	actor = harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorPhaseFailed, actor.Status.Phase)
	assert.Equal(t, workspacesv1alpha1.ActorReasonRunnerFailed, actor.Status.Conditions[0].Reason)
}

func TestActorDoesNotCreateASecondRunAfterACrash(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.reconcile()

	// The status update after CreateRun was lost.
	actor := harness.actor()
	actor.Status.RunID = ""
	actor.Status.Phase = workspacesv1alpha1.ActorPhaseStarting
	require.NoError(t, harness.client.Status().Update(context.Background(), actor))
	harness.reconcile()

	assert.Len(t, harness.daemon.created, 1)
	assert.Equal(t, []string{testActorRunner}, harness.daemon.minted)
	assert.Equal(t, testRunID, harness.actor().Status.RunID)
}

func TestActorRefusesAProfileThatPicksRunners(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	actor := harness.actor()
	actor.Spec.Profile.Inline = "actor:\n  runner: somewhere\n"
	require.NoError(t, harness.client.Update(context.Background(), actor))
	harness.reconcile()

	actor = harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorPhaseFailed, actor.Status.Phase)
	assert.Equal(t, workspacesv1alpha1.ActorReasonInvalidProfile, actor.Status.Conditions[0].Reason)
	assert.Empty(t, harness.daemon.minted)
	assert.Empty(t, harness.daemon.created)
}

func TestActorRevokesTheCredentialWhenPlacementFails(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.daemon.placement = assert.AnError
	harness.reconcile()
	assert.Equal(t, workspacesv1alpha1.ActorPhaseFailed, harness.actor().Status.Phase)
	assert.Empty(t, harness.daemon.created)

	// The runner Pod started before placement was checked; the credential is
	// revoked once the Pod is gone.
	harness.reconcile()
	assert.Equal(t, []string{testActorRunner}, harness.daemon.revoked)
	harness.now = harness.now.Add(actorPodLinger)
	harness.reconcile()
	harness.reconcile()
	assert.Equal(t, []string{testActorRunner, testActorRunner}, harness.daemon.revoked)
}

func TestActorFollowsTheRunAndRevokesAfterThePodEnds(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.reconcile()

	exit := int32(0)
	harness.daemon.runs[testRunID] = needy.Run{
		ID:         testRunID,
		State:      needyv1.RunState_RUN_STATE_FINISHED,
		ActorState: needyv1.ActorState_ACTOR_STATE_EXITED,
		ExitCode:   &exit,
	}
	harness.reconcile()
	actor := harness.actor()
	assert.Equal(t, workspacesv1alpha1.ActorPhaseSucceeded, actor.Status.Phase)
	assert.Equal(t, "FINISHED", actor.Status.RunState)
	assert.Equal(t, "EXITED", actor.Status.ActorState)
	require.NotNil(t, actor.Status.CompletedAt)

	// While the runner Pod runs, its credential stays: a refused credential
	// makes the ephemeral runner exit with an error. The first revoke freed
	// the name before minting.
	harness.reconcile()
	assert.Equal(t, []string{testActorRunner}, harness.daemon.revoked)

	pod := harness.pod()
	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, harness.client.Status().Update(context.Background(), pod))
	harness.reconcile()
	assert.Equal(t, []string{testActorRunner, testActorRunner}, harness.daemon.revoked)
	assert.Equal(t, testTrueValue, harness.actor().Annotations[actorCredentialRevokedAnnotation])
}

func TestActorDeletesALingeringRunnerPod(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.reconcile()
	harness.daemon.runs[testRunID] = needy.Run{
		ID:    testRunID,
		State: needyv1.RunState_RUN_STATE_FAILED,
	}
	harness.reconcile()
	assert.Equal(t, workspacesv1alpha1.ActorPhaseFailed, harness.actor().Status.Phase)

	harness.now = harness.now.Add(actorPodLinger)
	harness.reconcile()
	var pod corev1.Pod
	err := harness.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testActorName + "-runner"}, &pod)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestActorStopsAtMaxRunDuration(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.reconcile()

	harness.now = harness.now.Add(time.Hour)
	harness.reconcile()
	assert.Equal(t, []string{testRunID}, harness.daemon.stopped)
	assert.Equal(t, workspacesv1alpha1.ActorStopReasonMaxRunDuration, harness.actor().Status.StopReason)

	harness.daemon.runs[testRunID] = needy.Run{
		ID:    testRunID,
		State: needyv1.RunState_RUN_STATE_STOPPED,
	}
	harness.reconcile()
	assert.Equal(t, workspacesv1alpha1.ActorPhaseStopped, harness.actor().Status.Phase)
	assert.Len(t, harness.daemon.stopped, 1)
}

func TestActorStopsWhenIdle(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	actor := harness.actor()
	actor.Spec.IdleTimeout = &metav1.Duration{Duration: 10 * time.Minute}
	require.NoError(t, harness.client.Update(context.Background(), actor))
	harness.reconcile()

	run := harness.daemon.runs[testRunID]
	run.LastActivity = harness.now.Add(5 * time.Minute)
	harness.daemon.runs[testRunID] = run
	harness.now = harness.now.Add(14 * time.Minute)
	harness.reconcile()
	assert.Empty(t, harness.daemon.stopped)

	harness.now = harness.now.Add(time.Minute)
	harness.reconcile()
	assert.Equal(t, []string{testRunID}, harness.daemon.stopped)
	assert.Equal(t, workspacesv1alpha1.ActorStopReasonIdle, harness.actor().Status.StopReason)
}

func TestDeletingALiveActorStopsItsRun(t *testing.T) {
	harness := newActorHarness(t, executorWorkspace(), daemonClientSecret(), availableDaemon())
	harness.reconcile()

	require.NoError(t, harness.client.Delete(context.Background(), harness.actor()))
	harness.reconcile()
	assert.Equal(t, []string{testRunID}, harness.daemon.stopped)
	assert.Equal(t, []string{testActorRunner, testActorRunner}, harness.daemon.revoked)

	var actor workspacesv1alpha1.Actor
	err := harness.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testActorName}, &actor)
	assert.True(t, apierrors.IsNotFound(err))
}
