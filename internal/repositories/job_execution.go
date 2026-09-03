// Copyright 2026.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package repositories

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type jobExecutionOutcome[T client.Object] struct {
	object    T
	condition metav1.Condition
	jobName   string
	podNames  []string
	logError  error
}

// waitForJobExecution owns the polling and log-streaming protocol shared by
// RepositoryExec and WorktreeExec. Resource-specific clients only map their
// status shape and format the final domain error.
func waitForJobExecution[T client.Object](
	ctx context.Context,
	kubeClient client.Client,
	kubernetesClient kubernetes.Interface,
	key types.NamespacedName,
	newObject func() T,
	status func(T) (string, []metav1.Condition),
	conditionType string,
	resourceKind string,
	logDescription string,
	output io.Writer,
) (jobExecutionOutcome[T], error) {
	var result T
	if err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current := newObject()
		if err := kubeClient.Get(ctx, key, current); err != nil {
			return false, fmt.Errorf("get %s: %w", resourceKind, err)
		}
		_, conditions := status(current)
		condition := meta.FindStatusCondition(conditions, conditionType)
		if condition == nil || condition.Status == metav1.ConditionUnknown {
			return false, nil
		}
		result = current
		return true, nil
	}); err != nil {
		return jobExecutionOutcome[T]{}, err
	}

	jobName, conditions := status(result)
	condition := meta.FindStatusCondition(conditions, conditionType)
	outcome := jobExecutionOutcome[T]{object: result, condition: *condition, jobName: jobName}
	if jobName != "" {
		outcome.podNames, outcome.logError = writeJobLogs(ctx, kubernetesClient, result.GetNamespace(), jobName, logDescription, output)
	}
	return outcome, nil
}

func writeJobLogs(
	ctx context.Context,
	kubernetesClient kubernetes.Interface,
	namespace string,
	jobName string,
	description string,
	output io.Writer,
) ([]string, error) {
	pods, err := kubernetesClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return nil, fmt.Errorf("list %s Pods: %w", description, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("read %s logs: Job %q has no Pods", description, jobName)
	}

	slices.SortFunc(pods.Items, func(left, right corev1.Pod) int {
		return cmp.Compare(left.Name, right.Name)
	})

	podNames := make([]string, len(pods.Items))
	for index := range pods.Items {
		podNames[index] = pods.Items[index].Name
		stream, err := kubernetesClient.CoreV1().Pods(namespace).GetLogs(pods.Items[index].Name, &corev1.PodLogOptions{}).Stream(ctx)
		if err != nil {
			return podNames, fmt.Errorf("open %s logs for Pod %q: %w", description, pods.Items[index].Name, err)
		}

		_, copyErr := io.Copy(output, stream)
		closeErr := stream.Close()
		if copyErr != nil {
			return podNames, fmt.Errorf("write %s logs for Pod %q: %w", description, pods.Items[index].Name, copyErr)
		}
		if closeErr != nil {
			return podNames, fmt.Errorf("close %s logs for Pod %q: %w", description, pods.Items[index].Name, closeErr)
		}
	}

	return podNames, nil
}
