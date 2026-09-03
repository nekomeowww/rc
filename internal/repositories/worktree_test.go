package repositories

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

func TestWorktreeClientWaitWritesBootstrapLogsBeforeReturningFailure(t *testing.T) {
	t.Parallel()
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: execTestWorktree, Namespace: execTestNamespace},
		Status: repositoriesv1alpha1.WorktreeStatus{
			JobName: "feature-bootstrap",
			Conditions: []metav1.Condition{{
				Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionFalse,
				Reason: bootstrapFailedReason, Message: "Job reached its backoff limit",
			}},
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree).Build()
	logsWritten := false
	client := &WorktreeClient{
		Client: kubeClient,
		writeLogs: func(_ context.Context, namespace, jobName string, _ io.Writer) ([]string, error) {
			logsWritten = true
			assert.Equal(t, worktree.Namespace, namespace)
			assert.Equal(t, worktree.Status.JobName, jobName)
			return []string{"feature-bootstrap-pod"}, nil
		},
	}

	err := client.Wait(context.Background(), worktree, io.Discard)

	require.Error(t, err)
	assert.True(t, logsWritten, "write Bootstrap Pod logs")
	assert.ErrorContains(t, err, `Worktree "`+execTestWorktree+`", Job "feature-bootstrap", Pod "feature-bootstrap-pod"`)
	assert.ErrorContains(t, err, "Job reached its backoff limit")
}

func TestWorktreeExecClientStartPreservesArgv(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	execClient := &WorktreeExecClient{Client: kubeClient}

	exec, err := execClient.Start(context.Background(), WorktreeExecRequest{
		Namespace: execTestNamespace, Worktree: execTestWorktree, Command: []string{execTestGit, execTestStatus, execTestShort},
	})

	require.NoError(t, err)
	assert.NotEmpty(t, exec.Name)
	assert.Equal(t, execTestWorktree, exec.Spec.WorktreeRef.Name)
	assert.Equal(t, []string{execTestGit, execTestStatus, execTestShort}, exec.Spec.Command)
}

func TestWorktreeExecClientWaitPreservesJobLostConditionWhenLogsAreGone(t *testing.T) {
	t.Parallel()
	exec := &repositoriesv1alpha1.WorktreeExec{
		ObjectMeta: metav1.ObjectMeta{Name: "feature-exec", Namespace: execTestNamespace},
		Status: repositoriesv1alpha1.WorktreeExecStatus{
			JobName: "feature-exec",
			Conditions: []metav1.Condition{{
				Type: repositoriesv1alpha1.WorktreeExecConditionSucceeded, Status: metav1.ConditionFalse,
				Reason: "JobLost", Message: "Command Job disappeared before its terminal result was recorded",
			}},
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(exec).Build()
	execClient := &WorktreeExecClient{Client: kubeClient, Kubernetes: kubernetesfake.NewSimpleClientset()}

	err := execClient.Wait(context.Background(), exec, io.Discard)

	require.Error(t, err)
	assert.ErrorContains(t, err, "Command Job disappeared before its terminal result was recorded")
	assert.ErrorContains(t, err, "has no Pods")
}
