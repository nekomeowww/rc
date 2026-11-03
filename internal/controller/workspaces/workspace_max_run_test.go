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
	"errors"
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
)

type stubActivityReader struct {
	calls    int
	activity processruntime.Activity
	err      error
}

func (reader *stubActivityReader) LastActivity(context.Context, processruntime.Target) (processruntime.Activity, error) {
	reader.calls++
	return reader.activity, reader.err
}

// runningWorkspace is Ready since readySince, with a running runtime Pod.
func runningWorkspace(name string, readySince time.Time) (*workspacesv1alpha1.Workspace, *corev1.Pod) {
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testNamespace,
			Finalizers:        []string{workspaceFinalizer},
			CreationTimestamp: metav1.NewTime(readySince.Add(-time.Minute)),
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState: workspacesv1alpha1.WorkspaceDesiredStateRunning,
		},
		Status: workspacesv1alpha1.WorkspaceStatus{
			RuntimePodName: name,
			Conditions: []metav1.Condition{
				{
					Type:               workspacesv1alpha1.WorkspaceConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             "Ready",
					LastTransitionTime: metav1.NewTime(readySince),
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	return workspace, pod
}

func newRetentionTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	require.NoError(t, corev1.AddToScheme(scheme), "register core API types")
	statusObjects := make([]client.Object, 0, len(objects))
	for _, object := range objects {
		if _, ok := object.(*corev1.Pod); !ok {
			statusObjects = append(statusObjects, object)
		}
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).
		WithStatusSubresource(statusObjects...).
		WithObjects(objects...).
		Build()
}

func TestLifecycleMaxRunIgnoresActiveExecutions(t *testing.T) {
	t.Parallel()
	started := metav1.NewTime(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
	workspace := &workspacesv1alpha1.Workspace{
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState:   workspacesv1alpha1.WorkspaceDesiredStateRunning,
			IdleTimeout:    &metav1.Duration{Duration: time.Hour},
			MaxRunDuration: &metav1.Duration{Duration: 8 * time.Hour},
		},
		Status: workspacesv1alpha1.WorkspaceStatus{
			RunningSince:     &started,
			LastActivityTime: &started,
		},
	}

	deadlines := lifecycleDeadlines(workspace, workspaceProcesses{active: 1, any: true})

	assert.Nil(t, deadlines.idleSuspendAt, "an active execution blocks idle suspension")
	require.NotNil(t, deadlines.maxRunSuspendAt, "an active execution does not block the maximum run")
	assert.Equal(t, started.Add(8*time.Hour), *deadlines.maxRunSuspendAt)

	workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
	assert.Nil(t, lifecycleDeadlines(workspace, workspaceProcesses{}).maxRunSuspendAt, "a suspended Workspace has no run to bound")
}

func TestRetentionStopsWorkspaceAtMaxRunDuration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	readySince := time.Now().Add(-3 * time.Hour)
	workspace, pod := runningWorkspace("runaway", readySince)
	workspace.Spec.MaxRunDuration = &metav1.Duration{Duration: 2 * time.Hour}
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: testNamespace},
		Spec: workspacesv1alpha1.WorkspaceExecSpec{
			TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name},
			Command:   []string{testTrueValue},
		},
		Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseRunning},
	}
	kubeClient := newRetentionTestClient(t, workspace, pod, process)
	reconciler := &WorkspaceRetentionReconciler{Client: kubeClient, APIReader: kubeClient}

	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	require.NoError(t, err)

	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted))
	assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateSuspended, persisted.Spec.DesiredState, "suspend despite the running execution")
	require.NotNil(t, persisted.Status.Lifecycle)
	assert.Equal(t, workspacesv1alpha1.WorkspaceAutomaticSuspendMaxRunDuration, persisted.Status.Lifecycle.LastAutomaticSuspendReason)
	require.NotNil(t, persisted.Status.RunningSince, "the run clock starts at Ready")
	assert.WithinDuration(t, readySince, persisted.Status.RunningSince.Time, time.Second, "API times keep whole seconds")

	stopped := new(workspacesv1alpha1.WorkspaceExec)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(process), stopped))
	assert.Equal(t, workspacesv1alpha1.WorkspaceExecDesiredStateStopped, stopped.Spec.DesiredState, "stop the execution so compute can stop")
}

func TestRetentionPostponesIdleSuspensionForRuntimeActivity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace, pod := runningWorkspace("agent", time.Now().Add(-2*time.Hour))
	workspace.Spec.IdleTimeout = &metav1.Duration{Duration: time.Hour}
	kubeClient := newRetentionTestClient(t, workspace, pod)
	recent := time.Now().Add(-10 * time.Minute)
	activity := &stubActivityReader{activity: processruntime.Activity{LastActivity: &recent}}
	reconciler := &WorkspaceRetentionReconciler{Client: kubeClient, APIReader: kubeClient, Activity: activity}

	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	require.NoError(t, err)

	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted))
	assert.Equal(t, 1, activity.calls, "ask the supervisor once, at the deadline")
	assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateRunning, persisted.Spec.DesiredState, "recent tool calls keep the Workspace running")
	require.NotNil(t, persisted.Status.LastActivityTime)
	assert.WithinDuration(t, recent, persisted.Status.LastActivityTime.Time, time.Second, "runtime activity advances the idle clock")
	assert.InDelta(t, (50 * time.Minute).Seconds(), result.RequeueAfter.Seconds(), 5, "requeue at the new idle deadline")
}

func TestRetentionSuspendsIdleWorkspaceWithoutRuntimeActivity(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name     string
		activity *stubActivityReader
	}{
		{
			name:     "NoRecordedActivity",
			activity: &stubActivityReader{},
		},
		{
			name:     "SupervisorTooOld",
			activity: &stubActivityReader{err: processruntime.ErrActivityUnsupported},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			workspace, pod := runningWorkspace("idle", time.Now().Add(-2*time.Hour))
			workspace.Spec.IdleTimeout = &metav1.Duration{Duration: time.Hour}
			kubeClient := newRetentionTestClient(t, workspace, pod)
			reconciler := &WorkspaceRetentionReconciler{Client: kubeClient, APIReader: kubeClient, Activity: testCase.activity}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
			require.NoError(t, err)

			persisted := new(workspacesv1alpha1.Workspace)
			require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted))
			assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateSuspended, persisted.Spec.DesiredState)
			require.NotNil(t, persisted.Status.Lifecycle)
			assert.Equal(t, workspacesv1alpha1.WorkspaceAutomaticSuspendIdle, persisted.Status.Lifecycle.LastAutomaticSuspendReason)
		})
	}
}

func TestRetentionKeepsWorkspaceWhenActivityCannotBeRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace, pod := runningWorkspace("unreachable", time.Now().Add(-2*time.Hour))
	workspace.Spec.IdleTimeout = &metav1.Duration{Duration: time.Hour}
	kubeClient := newRetentionTestClient(t, workspace, pod)
	reconciler := &WorkspaceRetentionReconciler{
		Client:    kubeClient,
		APIReader: kubeClient,
		Activity:  &stubActivityReader{err: errors.New("exec failed")},
	}

	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	require.Error(t, err, "retry the activity query")

	persisted := new(workspacesv1alpha1.Workspace)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), persisted))
	assert.Equal(t, workspacesv1alpha1.WorkspaceDesiredStateRunning, persisted.Spec.DesiredState, "do not suspend work that may be in progress")
}
