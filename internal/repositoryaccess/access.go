// Package repositoryaccess coordinates access to a mutable Repository volume.
package repositoryaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Mode distinguishes mounts from clones: CSI requires an unused clone source.
type Mode string

const (
	Write           Mode = "write"
	Mount           Mode = "mount"
	Clone           Mode = "clone"
	stateAnnotation      = "repositories.rc.ayaka.io/access"
	gateLabel            = "repositories.rc.ayaka.io/access-gate"
)

// Gate persists reservations before their Job, Pod, or PVC is created. Updates
// use resourceVersion for atomic admission across controllers. Reservations do
// not expire: a timeout cannot fence a Pod that still accesses the volume.
// Owners release only after their consumer stops; finalizers cover deletion.
type Gate struct {
	Client client.Client
	Reader client.Reader
}

type reservation struct {
	Mode    Mode            `json:"mode"`
	Holders map[string]bool `json:"holders"`
}

func (g Gate) reader() client.Reader {
	if g.Reader != nil {
		return g.Reader
	}
	return g.Client
}

// Token identifies one resource incarnation, independent of name reuse.
func Token(kind string, owner client.Object) string {
	return kind + "/" + string(owner.GetUID()) + "/" + owner.GetName()
}

func leaseKey(repository *repositories.Repository) client.ObjectKey {
	sum := sha256.Sum256([]byte(string(repository.UID) + "/" + repository.Name))
	return client.ObjectKey{Namespace: repository.Namespace, Name: "rc-repository-" + hex.EncodeToString(sum[:10])}
}

func state(lease *coordinationv1.Lease) (reservation, error) {
	result := reservation{Holders: map[string]bool{}}
	if value := lease.Annotations[stateAnnotation]; value != "" {
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return result, fmt.Errorf("decode Repository reservation: %w", err)
		}
	}
	if result.Holders == nil {
		result.Holders = map[string]bool{}
	}
	return result, nil
}

// Acquire admits compatible consumers. When ready is required, readiness is
// re-read without the manager cache after admission, before creating a consumer.
// A changed spec or status invalidates the captured Repository state. In
// particular, an old bootstrap result cannot overwrite a newer sync result.
func (g Gate) Acquire(ctx context.Context, repository *repositories.Repository, token string, mode Mode, ready bool) (bool, error) {
	acquired := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		acquired = false
		lease := new(coordinationv1.Lease)
		key := leaseKey(repository)
		err := g.reader().Get(ctx, key, lease)
		if errors.IsNotFound(err) {
			lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace,
				Labels:          map[string]string{gateLabel: "true"},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: repositories.SchemeGroupVersion.String(), Kind: "Repository", Name: repository.Name, UID: repository.UID}}}}
			if err := g.Client.Create(ctx, lease); err != nil && !errors.IsAlreadyExists(err) {
				return err
			}
			if err := g.reader().Get(ctx, key, lease); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		current, err := state(lease)
		if err != nil {
			return err
		}
		if current.Holders[token] {
			acquired = true
			return nil
		}
		if len(current.Holders) > 0 && (mode == Write || current.Mode != mode) {
			return nil
		}
		current.Mode = mode
		current.Holders[token] = true
		encoded, err := json.Marshal(current)
		if err != nil {
			return err
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		lease.Annotations[stateAnnotation] = string(encoded)
		if err := g.Client.Update(ctx, lease); err != nil {
			return err
		}
		acquired = true
		return nil
	})
	if err != nil || !acquired {
		return false, err
	}
	current := new(repositories.Repository)
	if err := g.reader().Get(ctx, client.ObjectKeyFromObject(repository), current); err != nil {
		return false, err
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, repositories.RepositoryConditionStorageReady)
	if current.UID != repository.UID || current.Generation != repository.Generation || !equality.Semantic.DeepEqual(current.Status, repository.Status) || !current.DeletionTimestamp.IsZero() ||
		(ready && (current.Status.ObservedGeneration != current.Generation || condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != current.Generation || current.Status.VolumeClaimName == "")) {
		return false, g.Release(ctx, repository.Namespace, token)
	}
	return g.consumersStopped(ctx, repository, mode)
}

// consumersStopped protects admission across upgrades and observes consumers
// outside rc without treating a cached absence as proof that they stopped.
func (g Gate) consumersStopped(ctx context.Context, repository *repositories.Repository, mode Mode) (bool, error) {
	// Also cover consumers created before the access protocol was installed.
	// Direct Kubernetes access is outside this protocol; rc never deletes it.
	if mode == Write || mode == Clone {
		pods := new(corev1.PodList)
		if err := g.reader().List(ctx, pods, client.InNamespace(repository.Namespace)); err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			for _, volume := range pod.Spec.Volumes {
				if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == repository.Status.VolumeClaimName {
					return false, nil
				}
			}
		}
	}
	if mode == Write {
		claims := new(corev1.PersistentVolumeClaimList)
		if err := g.reader().List(ctx, claims, client.InNamespace(repository.Namespace)); err != nil {
			return false, err
		}
		for _, claim := range claims.Items {
			source := claim.Spec.DataSource
			if source != nil && source.Kind == "PersistentVolumeClaim" && source.Name == repository.Status.VolumeClaimName && claim.Status.Phase != corev1.ClaimBound {
				return false, nil
			}
		}
	}
	return true, nil
}

// Release removes this owner's reservations in a namespace. Consumers must be
// stopped first. Keeping empty gates makes name reuse and concurrent CAS safe.
func (g Gate) Release(ctx context.Context, namespace, token string) error {
	leases := new(coordinationv1.LeaseList)
	if err := g.reader().List(ctx, leases, client.InNamespace(namespace), client.MatchingLabels{gateLabel: "true"}); err != nil {
		return err
	}
	for _, item := range leases.Items {
		key := client.ObjectKeyFromObject(&item)
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			lease := new(coordinationv1.Lease)
			if err := g.reader().Get(ctx, key, lease); err != nil {
				return client.IgnoreNotFound(err)
			}
			current, err := state(lease)
			if err != nil {
				return err
			}
			if !current.Holders[token] {
				return nil
			}
			delete(current.Holders, token)
			encoded, err := json.Marshal(current)
			if err != nil {
				return err
			}
			lease.Annotations[stateAnnotation] = string(encoded)
			return g.Client.Update(ctx, lease)
		}); err != nil {
			return err
		}
	}
	return nil
}

// Busy reports a reservation held by another consumer. Bootstrap uses this
// before publishing readiness so it cannot overwrite a running sync's status.
func (g Gate) Busy(ctx context.Context, repository *repositories.Repository, token string) (bool, error) {
	lease := new(coordinationv1.Lease)
	if err := g.reader().Get(ctx, leaseKey(repository), lease); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	current, err := state(lease)
	if err != nil {
		return false, err
	}
	return len(current.Holders) > 0 && !current.Holders[token], nil
}
