package worktrees

import (
	"context"
	"io"
	"testing"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	clioutput "github.com/nekomeowww/rc/internal/cli/rcctl/output"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const listTestNamespace = "development"

func TestAddHasNoPositionalArgumentsAndAcceptsGitStyleFlags(t *testing.T) {
	t.Parallel()
	command := newAddCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{
		"--repo", "gitlab.com/acme/tools",
		"-b", "feature/login",
		"--name", "tools-feature-login",
		"--detach=false",
	}))
	require.NoError(t, command.Args(command, command.Flags().Args()))
}

func TestWorktreeListRowsReportStatusAndSortByName(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	worktrees := []repositoriesv1alpha1.Worktree{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "zeta-main"},
			Spec:       repositoriesv1alpha1.WorktreeSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: "zeta"}},
			Status: repositoriesv1alpha1.WorktreeStatus{
				VolumeClaimName: "zeta-main", WorktreePath: "/repository/worktree",
				Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue}},
			},
		},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha-main"}},
	}

	rows := worktreeListRows(worktrees)

	require.Len(t, rows, 2, "create one row per Worktree")
	assertions.Equal("alpha-main", rows[0].name, "sort rows by Worktree name")
	assertions.False(rows[0].ready, "report an absent Ready condition as false")
	assertions.Equal("-", rows[0].repository, "render an unavailable Repository explicitly")
	assertions.Equal("zeta-main", rows[1].name, "retain the second sorted Worktree")
	assertions.True(rows[1].ready, "report Ready=True")
	assertions.Equal("zeta", rows[1].repository, "report the source Repository")
	assertions.Equal("zeta-main", rows[1].volume, "report the child PVC")
	assertions.Equal("/repository/worktree", rows[1].path, "report the native worktree path")
}

func TestWorktreeListReturnsKubernetesAPIError(t *testing.T) {
	t.Parallel()
	kubeClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	err := runWorktreeList(context.Background(), io.Discard, kubeClient, listTestNamespace, clioutput.Options{})

	require.Error(t, err, "listing an unregistered Worktree type fails")
	assert.ErrorContains(t, err, "list Worktrees", "identify the failed resource operation")
}

func TestWorktreeListCommandHasAliasAndRejectsArguments(t *testing.T) {
	t.Parallel()
	command := newListCommand(kubeconfig.NewFlags())

	assert.Contains(t, command.Aliases, "ls", "offer the conventional list alias")
	require.NotNil(t, command.Flag("output"), "list accepts an output format")
	require.Error(t, command.Args(command, []string{"unexpected"}), "list accepts no positional arguments")
}

func TestWorktreeGetCommandRequiresNameAndSupportsStructuredOutput(t *testing.T) {
	t.Parallel()
	command := newGetCommand(kubeconfig.NewFlags())

	require.NotNil(t, command.Flag("output"), "get accepts a structured output format")
	require.NoError(t, command.Args(command, []string{"example-main"}), "get accepts exactly one Worktree name")
	require.Error(t, command.Args(command, nil), "get requires a Worktree name")
}
