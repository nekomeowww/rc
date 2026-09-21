package repositories

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

const (
	testCheckoutRef  = "origin/main"
	testWorktreeName = "review"
)

func TestWorktreeCheckoutArgs(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{gitCheckoutSubcommand, "-b", "feature/new"}, worktreeCheckoutArgs(&repositoriesv1alpha1.Worktree{
		Spec: repositoriesv1alpha1.WorktreeSpec{Branch: "feature/new"},
	}))
	assert.Equal(t, []string{gitCheckoutSubcommand, "-B", "feature/reset", testCheckoutRef}, worktreeCheckoutArgs(&repositoriesv1alpha1.Worktree{
		Spec: repositoriesv1alpha1.WorktreeSpec{ResetBranch: "feature/reset", Ref: testCheckoutRef},
	}))
	assert.Equal(t, []string{gitCheckoutSubcommand, "--detach", "0123456789abcdef"}, worktreeCheckoutArgs(&repositoriesv1alpha1.Worktree{
		Spec: repositoriesv1alpha1.WorktreeSpec{Detach: true, Ref: "0123456789abcdef"},
	}))
	assert.Equal(t, []string{gitCheckoutSubcommand, "--orphan", testWorktreeName, testCheckoutRef}, worktreeCheckoutArgs(&repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: testWorktreeName},
		Spec:       repositoriesv1alpha1.WorktreeSpec{Orphan: true, Ref: testCheckoutRef},
	}))
	assert.Equal(t, []string{gitCheckoutSubcommand, "-b", testWorktreeName}, worktreeCheckoutArgs(&repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: testWorktreeName},
	}))
}

func TestWorktreePathPreservesExistingCheckoutLocation(t *testing.T) {
	t.Parallel()

	assert.Equal(t, workerMountPath, worktreePath(&repositoriesv1alpha1.Worktree{}))
	assert.Equal(t, "/repository/worktree/legacy", worktreePath(&repositoriesv1alpha1.Worktree{
		Status: repositoriesv1alpha1.WorktreeStatus{WorktreePath: "/repository/worktree/legacy"},
	}))
}
