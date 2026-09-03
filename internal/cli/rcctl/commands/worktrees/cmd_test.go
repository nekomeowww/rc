package worktrees

import (
	"context"
	"io"
	"testing"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	clioutput "github.com/nekomeowww/rc/pkg/output"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
)

const (
	listTestNamespace = "development"
	testWorktreeName  = "feature"
	testExecGit       = "git"
	testExecStatus    = "status"
	testExecShort     = "--short"
)

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

func TestWorktreeDeleteCommandAliases(t *testing.T) {
	t.Parallel()
	command := newDeleteCommand(kubeconfig.NewFlags())

	assert.Contains(t, command.Aliases, "remove", "offer the long deletion alias")
	assert.Contains(t, command.Aliases, "rm", "offer the conventional short deletion alias")
	require.NoError(t, command.Args(command, []string{"example-main"}), "delete accepts exactly one Worktree name")
	require.Error(t, command.Args(command, nil), "delete requires a Worktree name")
}

func TestRunWorktreeDeleteRejectsMountedWorktree(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	requirements.NoError(coordinationv1.AddToScheme(scheme), "register coordination API types")
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: testWorktreeName, Namespace: listTestNamespace}}
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: listTestNamespace},
		Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{
			Name: testWorktreeName, Path: testWorktreeName, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name},
		}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree, workspace).Build()

	err := runWorktreeDelete(context.Background(), kubeClient, listTestNamespace, worktree.Name)

	requirements.Error(err, "reject deletion while a Workspace references the Worktree")
	assertions.ErrorContains(err, `mounted by Workspace "dev"`, "identify the blocking Workspace")
	persisted := new(repositoriesv1alpha1.Worktree)
	requirements.NoError(kubeClient.Get(context.Background(), types.NamespacedName{
		Name: worktree.Name, Namespace: worktree.Namespace,
	}, persisted), "retain a mounted Worktree")
}

func TestRunWorktreeDeleteRequestsProtectedDeletion(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	require.NoError(t, coordinationv1.AddToScheme(scheme), "register coordination API types")
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: testWorktreeName, Namespace: listTestNamespace}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree).Build()

	require.NoError(t, runWorktreeDelete(context.Background(), kubeClient, worktree.Namespace, worktree.Name))
	persisted := new(repositoriesv1alpha1.Worktree)
	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{Name: worktree.Name, Namespace: worktree.Namespace}, persisted))
	assert.False(t, persisted.DeletionTimestamp.IsZero(), "request deletion from the API server")
	assert.Contains(t, persisted.Finalizers, worktreeclaim.DeletionFinalizer, "retain server-side deletion protection for the controller")
}

func TestRunWorktreeDeleteRejectsWriterThatWinsTheClaimRace(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme), "register Repository API types")
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme), "register Workspace API types")
	require.NoError(t, coordinationv1.AddToScheme(scheme), "register coordination API types")
	worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{
		Name: testWorktreeName, Namespace: listTestNamespace, UID: "worktree-uid",
	}}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree).Build()
	foreignHolder := "exec-uid"
	competitor := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: worktreeclaim.LeaseName(worktree), Namespace: worktree.Namespace, Labels: map[string]string{worktreeclaim.HolderLabel: "active-exec"}},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &foreignHolder},
	}
	kubeClient := &deletionRaceClient{Client: baseClient, competitor: competitor}

	err := runWorktreeDelete(context.Background(), kubeClient, worktree.Namespace, worktree.Name)

	require.Error(t, err, "reject deletion when a writer atomically claims the Worktree first")
	assert.ErrorContains(t, err, "active-exec", "identify the writer that won the Lease race")
	persisted := new(repositoriesv1alpha1.Worktree)
	require.NoError(t, baseClient.Get(context.Background(), types.NamespacedName{Name: worktree.Name, Namespace: worktree.Namespace}, persisted))
}

type deletionRaceClient struct {
	client.Client
	competitor *coordinationv1.Lease
	injected   bool
}

func (c *deletionRaceClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if _, ok := object.(*coordinationv1.Lease); ok && !c.injected {
		c.injected = true
		if err := c.Client.Create(ctx, c.competitor.DeepCopy()); err != nil {
			return err
		}
	}
	return c.Client.Create(ctx, object, options...)
}

func TestWorktreeExecRequiresDelimiterAndPreservesArguments(t *testing.T) {
	t.Parallel()
	command := newExecCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{testWorktreeName, "--", testExecGit, testExecStatus, testExecShort}))
	args := command.Flags().Args()
	require.NoError(t, exactCommandArgs(command, args))
	assert.Equal(t, []string{testWorktreeName, testExecGit, testExecStatus, testExecShort}, args)
}

func TestWorktreeExecRejectsMissingDelimiter(t *testing.T) {
	t.Parallel()
	command := newExecCommand(kubeconfig.NewFlags())
	require.NoError(t, command.ParseFlags([]string{testWorktreeName, testExecGit, testExecStatus}))
	assert.EqualError(t, exactCommandArgs(command, command.Flags().Args()), "expected exactly one Worktree name before --")
}
