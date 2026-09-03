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
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type oneShotJobState int

type oneShotStatusAdapter[T client.Object] struct {
	newObject     func() T
	status        func(T) (string, []metav1.Condition)
	apply         func(T, string, []metav1.Condition)
	conditionType string
	resourceKind  string
}

const (
	oneShotJobAbsent oneShotJobState = iota
	oneShotJobPresent
	oneShotJobLost
	oneShotJobConflict
)

// observeOneShotJob distinguishes a command that has never been scheduled from
// one whose recorded Job disappeared. Recreating the latter could repeat an
// arbitrary side effect, so callers must terminate it as JobLost.
func observeOneShotJob(
	ctx context.Context,
	kubeClient client.Client,
	owner client.Object,
	recordedJobName string,
) (*batchv1.Job, oneShotJobState, error) {
	jobName := recordedJobName
	if jobName == "" {
		jobName = owner.GetName()
	}
	job := new(batchv1.Job)
	err := kubeClient.Get(ctx, client.ObjectKey{Name: jobName, Namespace: owner.GetNamespace()}, job)
	if err == nil {
		if !metav1.IsControlledBy(job, owner) {
			return job, oneShotJobConflict, nil
		}
		return job, oneShotJobPresent, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, oneShotJobAbsent, fmt.Errorf("get command Job: %w", err)
	}
	if recordedJobName != "" {
		return nil, oneShotJobLost, nil
	}
	return nil, oneShotJobAbsent, nil
}

func (a oneShotStatusAdapter[T]) set(
	ctx context.Context,
	kubeClient client.Client,
	key client.ObjectKey,
	status metav1.ConditionStatus,
	reason string,
	message string,
	jobName string,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := a.newObject()
		if err := kubeClient.Get(ctx, key, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		currentJobName, currentConditions := a.status(current)
		if condition := meta.FindStatusCondition(currentConditions, a.conditionType); condition != nil && condition.Status != metav1.ConditionUnknown {
			return nil
		}
		before, ok := current.DeepCopyObject().(T)
		if !ok {
			return fmt.Errorf("copy %s status object", a.resourceKind)
		}
		conditions := append([]metav1.Condition(nil), currentConditions...)
		meta.SetStatusCondition(&conditions, metav1.Condition{
			Type:               a.conditionType,
			Status:             status,
			ObservedGeneration: current.GetGeneration(),
			Reason:             reason,
			Message:            message,
		})
		a.apply(current, jobName, conditions)
		if currentJobName == jobName && conditionsEqual(currentConditions, conditions) {
			return nil
		}
		if err := kubeClient.Status().Patch(ctx, current, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("patch %s status: %w", a.resourceKind, err)
		}
		return nil
	})
}

func terminalJobOutcome(job *batchv1.Job) (metav1.ConditionStatus, string, string, bool) {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return metav1.ConditionTrue, "CommandSucceeded", "Command completed successfully", true
		case batchv1.JobFailed:
			message := condition.Message
			if message == "" {
				message = "Command failed"
			}
			return metav1.ConditionFalse, "CommandFailed", message, true
		}
	}
	return metav1.ConditionUnknown, "", "", false
}
