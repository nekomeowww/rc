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
	var result *repositoriesv1alpha1.RepositoryExec

	if err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current := new(repositoriesv1alpha1.RepositoryExec)
		if err := c.Get(ctx, types.NamespacedName{Name: exec.Name, Namespace: exec.Namespace}, current); err != nil {
			return false, fmt.Errorf("get RepositoryExec: %w", err)
		}
		condition := meta.FindStatusCondition(current.Status.Conditions, repositoriesv1alpha1.RepositoryExecConditionSucceeded)
		if condition == nil || condition.Status == metav1.ConditionUnknown {
			return false, nil
		}
		result = current
		return true, nil
	}); err != nil {
		return err
	}
	if result.Status.JobName != "" {
		if err := c.writeJobLogs(ctx, result.Namespace, result.Status.JobName, output); err != nil {
			return err
		}
	}

	condition := meta.FindStatusCondition(result.Status.Conditions, repositoriesv1alpha1.RepositoryExecConditionSucceeded)
	if condition.Status != metav1.ConditionTrue {
		return fmt.Errorf("repository exec failed: %s", condition.Message)
	}

	return nil
}

func (c *ExecClient) writeJobLogs(ctx context.Context, namespace, jobName string, output io.Writer) error {
	pods, err := c.Kubernetes.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return fmt.Errorf("list Repository Exec Pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("read Repository Exec logs: Job %q has no Pods", jobName)
	}

	slices.SortFunc(pods.Items, func(left, right corev1.Pod) int {
		return cmp.Compare(left.Name, right.Name)
	})

	for index := range pods.Items {
		stream, err := c.Kubernetes.CoreV1().Pods(namespace).GetLogs(pods.Items[index].Name, &corev1.PodLogOptions{}).Stream(ctx)
		if err != nil {
			return fmt.Errorf("open Repository Exec logs for Pod %q: %w", pods.Items[index].Name, err)
		}

		_, copyErr := io.Copy(output, stream)
		closeErr := stream.Close()
		if copyErr != nil {
			return fmt.Errorf("write Repository Exec logs for Pod %q: %w", pods.Items[index].Name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close Repository Exec logs for Pod %q: %w", pods.Items[index].Name, closeErr)
		}
	}

	return nil
}
