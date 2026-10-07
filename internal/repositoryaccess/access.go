// Package repositoryaccess coordinates access to a mutable Repository volume.
package repositoryaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/holdset"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Mode distinguishes mounts from clones: CSI requires an unused clone source.
type Mode = holdset.Mode

// Admission describes the reservation after a successful Acquire call.
// Only Admitted permits creation of a Job, Pod, or clone PVC.
type Admission int

const (
	// NotReserved means this holder has no reservation on the requested Repository.
	NotReserved Admission = iota
	// Reserved blocks incompatible callers while existing consumers stop.
	// The owner must retry admission or release after its consumers stop.
	Reserved
	// Admitted holds the reservation and permits creation of the consumer.
	// The owner keeps it until that consumer stops or its clone PVC is Bound.
	Admitted
)

const (
	Write Mode = "write"
	Mount Mode = "mount"
	Clone Mode = "clone"
	// StateAnnotation stores the hold set on a Repository access Lease.
	StateAnnotation = "repositories.rc.ayaka.io/access"
	gateLabel       = "repositories.rc.ayaka.io/access-gate"
)

// Holder kinds are the Kubernetes kinds of Repository consumers.
const (
	KindWorkspace      = "Workspace"
	KindWorktree       = "Worktree"
	KindRepositorySync = "RepositorySync"
	KindRepositoryExec = "RepositoryExec"
	KindRepository     = "Repository"
)

// legacyRoles maps the role prefix of pre-holdset tokens to holder kinds.
var legacyRoles = map[string]string{
	"workspace": KindWorkspace, "clone": KindWorktree, "sync": KindRepositorySync,
	"repository-exec": KindRepositoryExec, "bootstrap": KindRepository,
}

// Gate persists reservations before their Job, Pod, or PVC is created. Updates
// use resourceVersion for atomic admission across controllers. Reservations do
// not expire: a timeout cannot fence a Pod that still accesses the volume.
// Owners release only after their consumer stops; finalizers cover deletion.
type Gate struct {
	Client client.Client
	// Reader is required and must bypass the manager cache in production.
	Reader client.Reader
}

// Holder identifies one consumer incarnation, independent of name reuse.
func Holder(kind string, owner client.Object, mode Mode) holdset.Holder {
	return holdset.HolderFor(kind, owner, mode)
}

// Conflicts admits any number of mounts or clones, but never mixed with each
// other or with a writer. Writers are exclusive.
func Conflicts(existing []holdset.Holder, candidate holdset.Holder) []holdset.Holder {
	var blocking []holdset.Holder
	for _, holder := range existing {
		if candidate.Mode == Write || holder.Mode != candidate.Mode {
			blocking = append(blocking, holder)
		}
	}
	return blocking
}

// Decode reads a Repository access Lease, including the pre-holdset encoding
// {"mode":M,"holders":{"role/uid/name":true}} written by older controllers.
func Decode(lease client.Object) (holdset.State, error) {
	value := lease.GetAnnotations()[StateAnnotation]
	var probe struct {
		Mode    Mode            `json:"mode"`
		Holders json.RawMessage `json:"holders"`
	}
	if value == "" {
		return holdset.State{}, nil
	}
	if err := json.Unmarshal([]byte(value), &probe); err != nil {
		return holdset.State{}, fmt.Errorf("decode Repository reservation: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(probe.Holders)), "{") {
		return holdset.Decode(lease, StateAnnotation)
	}
	tokens := map[string]bool{}
	if err := json.Unmarshal(probe.Holders, &tokens); err != nil {
		return holdset.State{}, fmt.Errorf("decode legacy Repository reservation: %w", err)
	}
	state := holdset.State{}
	for token, held := range tokens {
		role, rest, _ := strings.Cut(token, "/")
		uid, name, _ := strings.Cut(rest, "/")
		if !held || uid == "" {
			continue
		}
		kind := legacyRoles[role]
		if kind == "" {
			kind = role
		}
		state.Put(holdset.Holder{Kind: kind, Name: name, UID: types.UID(uid), Mode: probe.Mode, Since: lease.GetCreationTimestamp()})
	}
	return state, nil
}

// HeldBy reports whether the lease's reservation holds any holder, whatever
// its kind, for the resource incarnation with uid. A malformed reservation
// returns an error; callers that must fail closed treat that as held.
func HeldBy(lease *coordinationv1.Lease, uid types.UID) (bool, error) {
	if uid == "" {
		return false, nil
	}
	current, err := Decode(lease)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(current.Holders, func(h holdset.Holder) bool { return h.UID == uid }), nil
}

func leaseKey(repository *repositories.Repository) client.ObjectKey {
	sum := sha256.Sum256([]byte(string(repository.UID) + "/" + repository.Name))
	return client.ObjectKey{Namespace: repository.Namespace, Name: "rc-repository-" + hex.EncodeToString(sum[:10])}
}

// Store returns the hold set of this Repository incarnation.
func (g Gate) Store(repository *repositories.Repository) holdset.LeaseStore {
	return holdset.LeaseStore{
		Client: g.Client, Reader: g.Reader, Key: leaseKey(repository), Annotation: StateAnnotation, Decode: Decode,
		Labels:          map[string]string{gateLabel: "true"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: repositories.SchemeGroupVersion.String(), Kind: "Repository", Name: repository.Name, UID: repository.UID}},
	}
}

// Acquire reserves parent access before checking existing consumers. Reserved
// retains the reservation across retries without permission to create a consumer.
// Readiness and captured Repository state are checked outside the manager cache.
// On error, admission is unknown: an API write can succeed without a response.
// The owner must retry or Release after its consumers stop, even on error.
func (g Gate) Acquire(ctx context.Context, repository *repositories.Repository, holder holdset.Holder, ready bool) (Admission, error) {
	result, err := holdset.Acquire(ctx, g.Store(repository), holder, Conflicts)
	if err != nil || !result.Admitted {
		return NotReserved, err
	}
	current := new(repositories.Repository)
	if err := g.Reader.Get(ctx, client.ObjectKeyFromObject(repository), current); err != nil {
		return Reserved, err
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, repositories.RepositoryConditionStorageReady)
	if current.UID != repository.UID || current.Generation != repository.Generation || !sameState(current.Status, repository.Status) || !current.DeletionTimestamp.IsZero() ||
		(ready && (current.Status.ObservedGeneration != current.Generation || condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != current.Generation || current.Status.VolumeClaimName == "")) {
		return NotReserved, g.Release(ctx, repository, holder.Key())
	}
	stopped, err := g.consumersStopped(ctx, repository, holder.Mode)
	if err != nil || !stopped {
		return Reserved, err
	}
	return Admitted, nil
}

// ConsumersStopped reports that no Pod uses the parent volume and no pending
// clone reads it, read outside the cache. Sweep requires it before removing a
// reservation whose owner is gone.
func (g Gate) ConsumersStopped(ctx context.Context, repository *repositories.Repository) (bool, error) {
	return g.consumersStopped(ctx, repository, Write)
}

// sameState compares Repository status except the access mirror, which
// changes with every reservation and is never an input to admission.
func sameState(left, right repositories.RepositoryStatus) bool {
	left.Access, right.Access = nil, nil
	return equality.Semantic.DeepEqual(left, right)
}

// consumersStopped protects admission across upgrades and observes consumers
// outside rc without treating a cached absence as proof that they stopped.
func (g Gate) consumersStopped(ctx context.Context, repository *repositories.Repository, mode Mode) (bool, error) {
	// Also cover consumers created before the access protocol was installed.
	// Direct Kubernetes access is outside this protocol; rc never deletes it.
	if mode == Write || mode == Clone {
		pods := new(corev1.PodList)
		if err := g.Reader.List(ctx, pods, client.InNamespace(repository.Namespace)); err != nil {
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
		if err := g.Reader.List(ctx, claims, client.InNamespace(repository.Namespace)); err != nil {
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

// Release removes this holder from one Repository incarnation. Consumers must
// be stopped first. Keeping empty gates makes name reuse and concurrent CAS safe.
func (g Gate) Release(ctx context.Context, repository *repositories.Repository, key string) error {
	return holdset.Release(ctx, g.Store(repository), key)
}

// ReleaseNamed releases key from the current incarnation of a Repository named
// by an owner's spec. Earlier incarnations' Leases are garbage collected with them.
func (g Gate) ReleaseNamed(ctx context.Context, repository client.ObjectKey, key string) error {
	current := new(repositories.Repository)
	if err := g.Reader.Get(ctx, repository, current); err != nil {
		return client.IgnoreNotFound(err)
	}
	return g.Release(ctx, current, key)
}

// Busy reports a reservation held by another consumer. Bootstrap uses this
// before publishing readiness so it cannot overwrite a running sync's status.
func (g Gate) Busy(ctx context.Context, repository *repositories.Repository, key string) (bool, error) {
	_, current, err := g.Store(repository).Load(ctx)
	if err != nil {
		return false, err
	}
	_, held := current.Find(key)
	return len(current.Holders) > 0 && !held, nil
}
