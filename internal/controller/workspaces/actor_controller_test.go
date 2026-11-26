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
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/needy"
	needyv1 "github.com/nekomeowww/rc/internal/needy/gen/needy/v1"
)

const (
	testActorName    = "review"
	testActorUID     = "0123abcd-89ab-cdef-0123-456789abcdef"
	testActorRunner  = "actor-0123abcd89ab"
	testExecutorName = "dev"
	testKVMResource  = "example.com/kvm"
	testNeedyImage   = "example.com/needy:1"
	testKVMGroup     = int64(104)
	testRunID        = "run-1"
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

func (daemon *fakeDaemon) ActorRunnerConnected(_ context.Context, name string) (bool, error) {
	return !daemon.offline[name], nil
}

func (daemon *fakeDaemon) MintRunnerCredential(_ context.Context, name string, _ time.Duration) (string, error) {
	daemon.minted = append(daemon.minted, name)
	credential, err := needy.NewRunnerCredential(name)
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

func needyTestConfig() needy.Config {
	return needy.Config{
		Image:           testNeedyImage,
		KVMResource:     testKVMResource,
		KVMNodeSelector: map[string]string{testKVMResource: testTrueValue},
		KVMTolerations: []corev1.Toleration{
			{
				Key:      testKVMResource,
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			},
		},
		KVMGroup: testKVMGroup,
	}
}

func needyTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))

	return scheme
}

func executorWorkspace() *workspacesv1alpha1.Workspace {
	return &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: testExecutorName, Namespace: testNamespace, UID: "dev-uid"},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			Needy: &workspacesv1alpha1.WorkspaceNeedy{Executor: &workspacesv1alpha1.WorkspaceNeedyExecutor{}},
		},
	}
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
		ObjectMeta: metav1.ObjectMeta{Name: needy.DaemonName, Namespace: testNamespace},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
}

func daemonClientSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: needy.ClientSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{needy.ClientTokenKey: []byte("needy-client.rc.token")},
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
	scheme := needyTestScheme(t)
	objects = append(objects, testActor())
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&workspacesv1alpha1.Actor{}, &corev1.Pod{}).
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
	assert.Equal(t, harness.daemon.credentials[testActorRunner], string(credential.Data[needy.CredentialKey]))
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
	assert.Contains(t, command, "'--roles' 'actor' '--ephemeral'")
	assert.Contains(t, command, "'--gateway' 'tcp://needy-daemon."+testNamespace+".svc:7401'")
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
