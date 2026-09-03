package repositories

import (
	"context"
	"errors"
	"fmt"
	"io"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

// ExecRequest describes one exact argv to execute against a Repository.
type ExecRequest struct {
	Namespace  string
	Repository string
	Command    []string
}

// ExecClient creates RepositoryExec resources and observes their results.
type ExecClient struct {
	client.Client
	Kubernetes kubernetes.Interface
}

// Start creates a RepositoryExec request.
func (c *ExecClient) Start(ctx context.Context, request ExecRequest) (*repositoriesv1alpha1.RepositoryExec, error) {
	exec := &repositoriesv1alpha1.RepositoryExec{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: request.Repository + "-",
			Namespace:    request.Namespace,
		},
		Spec: repositoriesv1alpha1.RepositoryExecSpec{
			RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: request.Repository},
			Command:       append([]string(nil), request.Command...),
		},
	}

	err := c.Create(ctx, exec)
	if err != nil {
		return nil, fmt.Errorf("create RepositoryExec: %w", err)
	}

	return exec, nil
}

// Wait writes completed Job logs and returns the command's terminal result.
func (c *ExecClient) Wait(ctx context.Context, exec *repositoriesv1alpha1.RepositoryExec, output io.Writer) error {
	outcome, err := waitForJobExecution(ctx, c.Client, c.Kubernetes, types.NamespacedName{Name: exec.Name, Namespace: exec.Namespace},
		func() *repositoriesv1alpha1.RepositoryExec { return new(repositoriesv1alpha1.RepositoryExec) },
		func(current *repositoriesv1alpha1.RepositoryExec) (string, []metav1.Condition) {
			return current.Status.JobName, current.Status.Conditions
		}, repositoriesv1alpha1.RepositoryExecConditionSucceeded, "RepositoryExec", "Repository Exec", output)
	if err != nil {
		return err
	}
	if outcome.condition.Status != metav1.ConditionTrue {
		return errors.Join(fmt.Errorf("repository exec failed: %s", outcome.condition.Message), outcome.logError)
	}

	return outcome.logError
}
