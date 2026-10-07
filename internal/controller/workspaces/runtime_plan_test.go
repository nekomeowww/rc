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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const runtimePlanPodName = "runtime"

func TestTerminalRuntimePlanIgnoresOtherStates(t *testing.T) {
	require.Nil(t, planTerminalRuntime(nil, false))
	for _, phase := range []corev1.PodPhase{"", corev1.PodPending, corev1.PodRunning, corev1.PodUnknown} {
		pod := &corev1.Pod{Status: corev1.PodStatus{Phase: phase}}
		require.Nil(t, planTerminalRuntime(pod, false), "nonterminal lifecycle stays in Reconcile")
	}
}

func TestTerminalRuntimePlanWaitsForBoundExecutions(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: runtimePlanPodName, UID: "first"}, Status: corev1.PodStatus{Phase: corev1.PodFailed}}
	before := pod.DeepCopy()
	plan := planTerminalRuntime(pod, true)
	require.False(t, plan.replace)
	require.Equal(t, "RuntimeFailed", plan.reason)
	require.Contains(t, plan.message, "waiting for executions")
	require.True(t, planTerminalRuntime(pod, false).replace)
	require.Equal(t, before, pod, "planning must not mutate API observations")
}

func TestRuntimeDiagnosisRetainsGracefulEvictionAndInitializerExit(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: runtimePlanPodName, UID: "uid"},
		Spec:       corev1.PodSpec{NodeName: "disk-pressure-node"},
		Status: corev1.PodStatus{
			Phase:             corev1.PodSucceeded,
			Conditions:        []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "TerminationByKubelet", Message: "ephemeral-storage exhausted"}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: runtimeContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}}}},
		},
	}
	plan := planTerminalRuntime(pod, false)
	require.Equal(t, "RuntimeCompleted", plan.reason)
	require.Contains(t, plan.message, "TerminationByKubelet")
	require.Contains(t, plan.message, "ephemeral-storage")
	require.Contains(t, plan.message, "exitCode=0")
	require.Contains(t, plan.message, "disk-pressure-node")
	pod.Status.Phase = corev1.PodFailed
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "initialize", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 17}}}}
	plan = planTerminalRuntime(pod, false)
	require.True(t, plan.replace, "terminal initialization failure also gets recovery")
	require.Contains(t, plan.message, "initialize exitCode=17")
}
