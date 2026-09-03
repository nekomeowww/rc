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

package repositories

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

// WorktreeExecRequest describes one exact argv to execute against a Worktree.
type WorktreeExecRequest struct {
	Namespace string
	Worktree  string
	Command   []string
}

// WorktreeExecClient creates WorktreeExec resources and observes their Jobs.
type WorktreeExecClient struct {
	client.Client
	Kubernetes kubernetes.Interface
}

// Start creates a WorktreeExec request.
func (c *WorktreeExecClient) Start(ctx context.Context, request WorktreeExecRequest) (*repositoriesv1alpha1.WorktreeExec, error) {
	exec := &repositoriesv1alpha1.WorktreeExec{
		ObjectMeta: metav1.ObjectMeta{GenerateName: request.Worktree + "-", Namespace: request.Namespace},
		Spec: repositoriesv1alpha1.WorktreeExecSpec{
			WorktreeRef: repositoriesv1alpha1.WorktreeReference{Name: request.Worktree},
			Command:     append([]string(nil), request.Command...),
		},
	}
	if err := c.Create(ctx, exec); err != nil {
		return nil, fmt.Errorf("create WorktreeExec: %w", err)
	}
	return exec, nil
}

// Wait writes completed Job logs and returns the command's terminal result.
func (c *WorktreeExecClient) Wait(ctx context.Context, exec *repositoriesv1alpha1.WorktreeExec, output io.Writer) error {
	outcome, err := waitForJobExecution(ctx, c.Client, c.Kubernetes, types.NamespacedName{Name: exec.Name, Namespace: exec.Namespace},
		func() *repositoriesv1alpha1.WorktreeExec { return new(repositoriesv1alpha1.WorktreeExec) },
		func(current *repositoriesv1alpha1.WorktreeExec) (string, []metav1.Condition) {
			return current.Status.JobName, current.Status.Conditions
		}, repositoriesv1alpha1.WorktreeExecConditionSucceeded, "WorktreeExec", "Worktree Exec", output)
	if err != nil {
		return err
	}
	if outcome.condition.Status != metav1.ConditionTrue {
		location := fmt.Sprintf("worktree exec %q", outcome.object.GetName())
		if outcome.jobName != "" {
			location += fmt.Sprintf(" (Job %q", outcome.jobName)
			if len(outcome.podNames) > 0 {
				location += fmt.Sprintf(", Pod %q", strings.Join(outcome.podNames, `", "`))
			}
			location += ")"
		}
		return errors.Join(fmt.Errorf("%s failed: %s", location, outcome.condition.Message), outcome.logError)
	}
	return outcome.logError
}
