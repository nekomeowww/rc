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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// terminalRuntimePlan describes only terminal runtime diagnosis and replacement.
// The controller persists the diagnosis before cleaning mounts or deleting a Pod.
type terminalRuntimePlan struct {
	reason  string
	message string
	replace bool
}

// planTerminalRuntime returns nil for nonterminal Pods. A terminal runtime can
// be removed once executions bound to its UID have reached a terminal status;
// Pending, readiness, topology changes, and suspension stay in Reconcile.
func planTerminalRuntime(pod *corev1.Pod, boundActiveExec bool) *terminalRuntimePlan {
	if pod == nil || !runtimePodTerminal(pod) {
		return nil
	}
	plan := &terminalRuntimePlan{reason: "RuntimeFailed", message: runtimePodDiagnosis(pod), replace: !boundActiveExec}
	if pod.Status.Phase == corev1.PodSucceeded {
		plan.reason = "RuntimeCompleted"
	}
	if boundActiveExec {
		plan.message += "; waiting for executions bound to this Pod UID to become Lost"
	}
	return plan
}

func runtimePodTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
}

// runtimePodDiagnosis retains node disruptions even when a graceful supervisor
// exit produces Succeeded with no Pod-level reason. No workload logs are copied
// into status; transcripts remain on home storage and Pod logs need a log sink.
func runtimePodDiagnosis(pod *corev1.Pod) string {
	parts := []string{fmt.Sprintf("Pod %s (UID %s, node %s) phase=%s", pod.Name, pod.UID, pod.Spec.NodeName, pod.Status.Phase)}
	if pod.Status.Reason != "" || pod.Status.Message != "" {
		parts = append(parts, pod.Status.Reason+": "+pod.Status.Message)
	}
	for _, condition := range pod.Status.Conditions {
		if (condition.Type == corev1.DisruptionTarget && condition.Status == corev1.ConditionTrue) || (condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse) {
			parts = append(parts, condition.Reason+": "+condition.Message)
		}
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, status := range statuses {
			terminated := status.State.Terminated
			if terminated == nil {
				terminated = status.LastTerminationState.Terminated
			}
			if terminated != nil {
				parts = append(parts, fmt.Sprintf("container %s exitCode=%d signal=%d reason=%s finishedAt=%s: %s", status.Name, terminated.ExitCode, terminated.Signal, terminated.Reason, terminated.FinishedAt.UTC().Format(time.RFC3339), terminated.Message))
			}
			if waiting := status.State.Waiting; waiting != nil {
				parts = append(parts, status.Name+" "+waiting.Reason+": "+waiting.Message)
			}
		}
	}
	message := []rune(strings.Join(parts, "; "))
	// Leave room in the Condition message for cleanup progress appended by recovery.
	if len(message) > 8192 {
		return string(message[:8189]) + "..."
	}
	return string(message)
}
