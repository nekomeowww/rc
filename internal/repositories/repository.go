package repositories

import (
	"context"
	"fmt"
	"io"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

const bootstrapFailedReason = "BootstrapFailed"

// CloneRequest describes one Repository resource to create from a Git remote.
type CloneRequest struct {
	Namespace     string
	URL           string
	Name          string
	Ref           string
	StorageClass  string
	Size          resource.Quantity
	CredentialRef string
	// WithSubmodules initializes direct Git submodules. RecursiveSubmodules
	// implies WithSubmodules and also initializes nested submodules.
	WithSubmodules      bool
	RecursiveSubmodules bool
}

// RepositoryClient creates Repository resources and observes their bootstrap
// status.
type RepositoryClient struct {
	client.Client
}

// Clone creates a Repository resource. The controller performs the actual
// remote bootstrap into the Repository parent volume.
func (c *RepositoryClient) Clone(ctx context.Context, request CloneRequest) (*repositoriesv1alpha1.Repository, error) {
	derivedName, err := RepositoryName(request.URL)
	if err != nil {
		return nil, err
	}

	name := request.Name
	if name == "" {
		name = derivedName
	}

	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: request.Namespace,
		},
		Spec: repositoriesv1alpha1.RepositorySpec{
			Remote: repositoriesv1alpha1.RepositoryRemoteSpec{
				URL: request.URL,
			},
			Ref: request.Ref,
			Storage: repositoriesv1alpha1.RepositoryStorageSpec{
				StorageClassName: request.StorageClass,
				Size:             request.Size,
			},
		},
	}
	if request.CredentialRef != "" {
		repository.Spec.Remote.CredentialRef = &repositoriesv1alpha1.RepositoryCredentialReference{
			Name: request.CredentialRef,
		}
	}
	if request.WithSubmodules || request.RecursiveSubmodules {
		repository.Spec.Submodules = &repositoriesv1alpha1.RepositorySubmodulesSpec{
			Recursive: request.RecursiveSubmodules,
		}
	}

	if err := c.Create(ctx, repository); err != nil {
		return nil, fmt.Errorf("create Repository: %w", err)
	}

	return repository, nil
}

// Wait waits for the Repository bootstrap Job to make the parent volume ready.
func (c *RepositoryClient) Wait(ctx context.Context, repository *repositoriesv1alpha1.Repository, _ io.Writer) error {
	var result *repositoriesv1alpha1.Repository

	err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current := new(repositoriesv1alpha1.Repository)
		if err := c.Get(ctx, client.ObjectKeyFromObject(repository), current); err != nil {
			return false, fmt.Errorf("get Repository: %w", err)
		}

		condition := meta.FindStatusCondition(current.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady)
		if condition == nil || condition.Status == metav1.ConditionUnknown {
			return false, nil
		}
		if condition.Status == metav1.ConditionFalse {
			switch condition.Reason {
			case "VolumeClaimConflict", "StorageClassChangeUnsupported", "VolumeShrinkUnsupported", "CredentialNotFound", bootstrapFailedReason, "BootstrapJobConflict":
				return false, fmt.Errorf("repository failed: %s", condition.Message)
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
		return fmt.Errorf("repository did not produce a result")
	}

	return nil
}
