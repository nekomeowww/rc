package repositories

import (
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

// WorktreeAddRequest describes one independent child volume and native Git
// worktree request.
type WorktreeAddRequest struct {
	Namespace    string
	Repository   string
	Name         string
	Branch       string
	ResetBranch  string
	Ref          string
	Detach       bool
	Orphan       bool
	NoCheckout   bool
	Lock         bool
	LockReason   string
	StorageClass string
	Size         *resource.Quantity
	AccessModes  []corev1.PersistentVolumeAccessMode
}

// WorktreeClient creates Worktree resources and observes their bootstrap Jobs.
type WorktreeClient struct {
	client.Client
}

// Start resolves the Repository selector and creates a Worktree resource.
func (c *WorktreeClient) Start(ctx context.Context, request WorktreeAddRequest) (*repositoriesv1alpha1.Worktree, error) {
	repository, err := ResolveRepository(ctx, c.Client, request.Namespace, request.Repository)
	if err != nil {
		return nil, err
	}

	name := request.Name
	if name == "" {
		name = WorktreeName(repository.Name, request.Branch, request.Ref, request.Detach, request.Orphan)
	}

	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: request.Namespace},
		Spec: repositoriesv1alpha1.WorktreeSpec{
			RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repository.Name},
			Branch:        request.Branch,
			ResetBranch:   request.ResetBranch,
			Ref:           request.Ref,
			Detach:        request.Detach,
			Orphan:        request.Orphan,
			NoCheckout:    request.NoCheckout,
			Lock:          request.Lock,
			LockReason:    request.LockReason,
		},
	}
	if request.StorageClass != "" || request.Size != nil || len(request.AccessModes) > 0 {
		worktree.Spec.Storage = &repositoriesv1alpha1.WorktreeStorageSpec{
			StorageClassName: request.StorageClass,
			Size:             request.Size,
			AccessModes:      append([]corev1.PersistentVolumeAccessMode(nil), request.AccessModes...),
		}
	}

	err = c.Create(ctx, worktree)
	if err != nil {
		return nil, fmt.Errorf("create Worktree: %w", err)
	}

	return worktree, nil
}

// Wait waits for the native worktree bootstrap to complete.
func (c *WorktreeClient) Wait(ctx context.Context, worktree *repositoriesv1alpha1.Worktree, _ io.Writer) error {
	var result *repositoriesv1alpha1.Worktree

	err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current := new(repositoriesv1alpha1.Worktree)
		if err := c.Get(ctx, client.ObjectKeyFromObject(worktree), current); err != nil {
			return false, fmt.Errorf("get Worktree: %w", err)
		}

		condition := meta.FindStatusCondition(current.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
		if condition == nil || condition.Status == metav1.ConditionUnknown {
			return false, nil
		}
		if condition.Status == metav1.ConditionFalse {
			switch condition.Reason {
			case "RepositoryNotFound", "VolumeClaimConflict", "VolumeClaimSpecChanged", "BootstrapFailed", "BootstrapJobConflict":
				return false, fmt.Errorf("worktree failed: %s", condition.Message)
			}
			return false, nil
		}

		result = current

		return true, nil
	})
	if err != nil {
		return err
	}
	if result == nil {
		return fmt.Errorf("worktree did not produce a result")
	}

	return nil
}
