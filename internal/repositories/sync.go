package repositories

import (
	"context"
	"fmt"
	"time"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SyncClient submits independent requests and reads their durable results.
// A request never changes existing Worktree volumes.
type SyncClient struct{ client.Client }

// Start requests a fetch and reset using the Repository's configured credentials
// and ref. The controller captures configuration when it admits the request.
func (c *SyncClient) Start(ctx context.Context, namespace, repository string) (*repositoriesv1alpha1.RepositorySync, error) {
	request := &repositoriesv1alpha1.RepositorySync{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, GenerateName: repository + "-sync-"},
		Spec:       repositoriesv1alpha1.RepositorySyncSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repository}},
	}
	if err := c.Create(ctx, request); err != nil {
		return nil, fmt.Errorf("create RepositorySync: %w", err)
	}
	return request, nil
}

// Wait returns only this request's terminal result. Job logs are not required,
// so cleanup of a completed Job does not invalidate a successful request.
func (c *SyncClient) Wait(ctx context.Context, request *repositoriesv1alpha1.RepositorySync) (*repositoriesv1alpha1.RepositorySync, error) {
	current := new(repositoriesv1alpha1.RepositorySync)
	err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(request), current); err != nil {
			return false, err
		}
		if current.UID != request.UID {
			return false, fmt.Errorf("RepositorySync was replaced")
		}
		condition := meta.FindStatusCondition(current.Status.Conditions, repositoriesv1alpha1.RepositorySyncConditionSucceeded)
		if condition == nil || condition.ObservedGeneration != current.Generation || condition.Status == metav1.ConditionUnknown {
			return false, nil
		}
		if condition.Status != metav1.ConditionTrue {
			return false, fmt.Errorf("repository sync failed (%s): %s", condition.Reason, condition.Message)
		}
		return true, nil
	})
	return current, err
}
