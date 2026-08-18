package repositories

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

func TestExecClientStartPreservesArgv(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	execClient := &ExecClient{Client: kubeClient}

	exec, err := execClient.Start(context.Background(), ExecRequest{
		Namespace:  "development",
		Repository: "auv",
		Command:    []string{"git", "status", "--short"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, exec.Name)

	persisted := new(repositoriesv1alpha1.RepositoryExec)
	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{
		Name: exec.Name, Namespace: "development",
	}, persisted))
	assert.Equal(t, "auv", persisted.Spec.RepositoryRef.Name)
	assert.Equal(t, []string{"git", "status", "--short"}, persisted.Spec.Command)
}
